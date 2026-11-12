package executions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	enumspb "go.temporal.io/api/enums/v1"
)

type fixture struct {
	Admin, Work, Dispatch *pgxpool.Pool
	Ref                   Ref
	Principal, WorkflowID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	work, err := Open(ctx, os.Getenv("TEST_WORKFLOW_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := Open(ctx, os.Getenv("TEST_DISPATCHER_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{Admin: admin, Work: work, Dispatch: dispatch, Ref: Ref{TenantID: uuid.NewString(), ExecutionID: uuid.NewString()}, Principal: uuid.NewString()}
	f.WorkflowID = "lawpilot:" + f.Ref.TenantID + ":ai:" + f.Ref.ExecutionID
	t.Cleanup(func() {
		for _, table := range []string{"outbox_events", "audit_events", "ai_executions", "principal_capabilities", "memberships"} {
			admin.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id=$1", f.Ref.TenantID)
		}
		admin.Exec(ctx, "DELETE FROM tenants WHERE id=$1", f.Ref.TenantID)
		dispatch.Close()
		work.Close()
		admin.Close()
	})
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenants(id,name)VALUES($1,'synthetic fixture')", []any{f.Ref.TenantID}},
		{"INSERT INTO memberships(tenant_id,principal_id,status)VALUES($1,$2,'active')", []any{f.Ref.TenantID, f.Principal}},
		{"INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id)VALUES($1,$2,'intake.summarize.v1')", []any{f.Ref.TenantID, f.Principal}},
		{`INSERT INTO ai_executions(id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,status,request_input,workflow_id) VALUES($1,$2,$3,'test','hash','intake.summarize.v1','accepted','{"answers":{"value":"PRIVATE_TEST_DATA"}}',$4)`, []any{f.Ref.ExecutionID, f.Ref.TenantID, f.Principal, f.WorkflowID}},
		{`INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1,'lawpilot.ai.accepted.v1','1.0',$2,'{}')`, []any{f.Ref.TenantID, f.Ref.ExecutionID}},
	} {
		if _, err = admin.Exec(ctx, query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

type lostAck struct {
	calls, starts int
	ids           map[string]bool
}

func (s *lostAck) Start(_ context.Context, c Command) error {
	s.calls++
	if !s.ids[c.WorkflowID] {
		s.ids[c.WorkflowID] = true
		s.starts++
		return errors.New("lost acknowledgement")
	}
	return nil
}
func (s *lostAck) Describe(context.Context, string) (enumspb.WorkflowExecutionStatus, error) {
	return enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}
func TestDispatcherRestartAndLostAckDeduplicate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	starter := &lostAck{ids: map[string]bool{}}
	d := &Dispatcher{Pool: f.Dispatch, Store: &Store{Pool: f.Work}, Starter: starter, WorkerID: "first-process"}
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.Exec(ctx, "UPDATE outbox_events SET available_at=now() WHERE aggregate_id=$1", f.Ref.ExecutionID); err != nil {
		t.Fatal(err)
	}
	d.WorkerID = "restarted-process"
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	if err := f.Admin.QueryRow(ctx, "SELECT publish_state,delivery_attempts FROM outbox_events WHERE aggregate_id=$1", f.Ref.ExecutionID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "delivered" || attempts != 2 || starter.starts != 1 {
		t.Fatalf("state=%s attempts=%d starts=%d", state, attempts, starter.starts)
	}
}
func TestExpiredLeaseFencesStaleAcknowledgement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.Dispatch.Exec(ctx, "SELECT * FROM claim_execution_commands(NULL,NULL,NULL)"); err == nil {
		t.Fatal("null claim parameters accepted")
	}
	var event, token string
	if err := f.Dispatch.QueryRow(ctx, "SELECT event_id::text,lease_token::text FROM claim_execution_commands('one',1,10)").Scan(&event, &token); err != nil {
		t.Fatal(err)
	}
	f.Admin.Exec(ctx, "UPDATE outbox_events SET lease_expires_at=now()-interval '1 second' WHERE id=$1", event)
	var next string
	if err := f.Dispatch.QueryRow(ctx, "SELECT lease_token::text FROM claim_execution_commands('two',1,10)").Scan(&next); err != nil {
		t.Fatal(err)
	}
	var settled bool
	if err := f.Dispatch.QueryRow(ctx, "SELECT settle_execution_command($1,$2,NULL)", event, token).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled || next == token {
		t.Fatal("stale lease settled")
	}
	if _, err := f.Dispatch.Exec(ctx, "SELECT request_input FROM ai_executions"); err == nil {
		t.Fatal("dispatcher can read private execution inputs")
	}
}
func TestWorkerReauthorizesAndDoesNotRerunCommittedResult(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	calls := 0
	cap := Capability{ID: "intake.summarize.v1", AgentID: "intake-agent", AgentVersion: "0.1.0", RequiresAttorneyReview: true, MaxInputBytes: 8192}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"executionId": f.Ref.ExecutionID, "agentId": cap.AgentID, "agentVersion": cap.AgentVersion, "status": "abstained", "output": map[string]any{}, "evidence": []any{}, "proposedActions": []any{}, "requiresAttorneyReview": true})
	}))
	defer server.Close()
	a := &Activities{Store: &Store{Pool: f.Work}, Registry: map[string]Capability{cap.ID: cap}, SupervisorURL: server.URL, Secret: "test-secret-at-least-32-characters", HTTP: server.Client()}
	state, err := a.Run(ctx, f.Ref)
	if err != nil || state != "completed" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	var clean bool
	if err = f.Admin.QueryRow(ctx, "SELECT error_code IS NULL FROM ai_executions WHERE id=$1", f.Ref.ExecutionID).Scan(&clean); err != nil || !clean {
		t.Fatal("success retained an error code")
	}
	if _, err = a.Run(ctx, f.Ref); err != nil || calls != 1 {
		t.Fatal("repeated committed activity called supervisor")
	}
}
func TestRevokedGrantBlocksWorkerAndResponsePersistence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.Admin.Exec(ctx, "UPDATE memberships SET status='revoked' WHERE tenant_id=$1", f.Ref.TenantID)
	a := &Activities{Store: &Store{Pool: f.Work}}
	if _, err := a.Run(ctx, f.Ref); err == nil {
		t.Fatal("revoked grant allowed worker")
	}
	if _, err := a.Fail(ctx, Failure{Ref: f.Ref, Code: "PERMISSION_DENIED"}); err != nil {
		t.Fatal(err)
	}
	var status, code string
	if err := f.Admin.QueryRow(ctx, "SELECT status,error_code FROM ai_executions WHERE id=$1", f.Ref.ExecutionID).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "PERMISSION_DENIED" {
		t.Fatal("failure was not persisted")
	}
}

func TestRevocationWhileAgentRunsPreventsResultCommit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cap := Capability{ID: "intake.summarize.v1", AgentID: "intake-agent", AgentVersion: "0.1.0", RequiresAttorneyReview: true, MaxInputBytes: 8192}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := f.Admin.Exec(ctx, "UPDATE memberships SET status='revoked' WHERE tenant_id=$1", f.Ref.TenantID); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"executionId": f.Ref.ExecutionID, "agentId": cap.AgentID, "agentVersion": cap.AgentVersion, "status": "abstained", "output": map[string]any{"private": "should-not-be-stored"}, "evidence": []any{}, "proposedActions": []any{}, "requiresAttorneyReview": true})
	}))
	defer server.Close()
	a := &Activities{Store: &Store{Pool: f.Work}, Registry: map[string]Capability{cap.ID: cap}, SupervisorURL: server.URL, Secret: "test-secret-at-least-32-characters", HTTP: server.Client()}
	if _, err := a.Run(ctx, f.Ref); err == nil {
		t.Fatal("revoked response was committed")
	}
	var empty bool
	if err := f.Admin.QueryRow(ctx, "SELECT result IS NULL FROM ai_executions WHERE id=$1", f.Ref.ExecutionID).Scan(&empty); err != nil || !empty {
		t.Fatal("private result retained after revocation")
	}
}
func TestDeadLetterIsVisibleAndFenced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		var event, token string
		if err := f.Dispatch.QueryRow(ctx, "SELECT event_id::text,lease_token::text FROM claim_execution_commands('failing-dispatcher',1,10)").Scan(&event, &token); err != nil {
			t.Fatal(err)
		}
		var settled bool
		if err := f.Dispatch.QueryRow(ctx, "SELECT settle_execution_command($1,$2,'untrusted error text')", event, token).Scan(&settled); err != nil || !settled {
			t.Fatal("failed delivery not recorded")
		}
		f.Admin.Exec(ctx, "UPDATE outbox_events SET available_at=now() WHERE id=$1", event)
	}
	var state, code string
	if err := f.Admin.QueryRow(ctx, "SELECT publish_state,last_error_code FROM outbox_events WHERE aggregate_id=$1", f.Ref.ExecutionID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "dead_letter" || code != "TEMPORAL_UNAVAILABLE" {
		t.Fatalf("state=%s code=%s", state, code)
	}
}
