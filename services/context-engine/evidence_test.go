package contextengine

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"testing"
	"time"
)

func TestAuthoritativeEvidenceGroundingAndSourceUpdateLock(t *testing.T) {
	f := fixtureFor(t)
	doc := f.ingest(t, "Lease agreement. Rent is due monthly.")
	pkg := f.build(t, "lease")
	c := pkg.Sources[0].Citation
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := f.admin.Exec(ctx, "INSERT INTO ai_executions(id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,matter_id,status,request_input,workflow_id)VALUES($1,$2,$3,$4,$5,'matter.qa.v1',$6,'running','{}',$7)", id, f.tenant, f.who, uuid.NewString(), digest("synthetic"), f.matter, "synthetic:"+id); err != nil {
		t.Fatal(err)
	}
	citationBytes, _ := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(c)
	var citation any
	json.Unmarshal(citationBytes, &citation)
	output := map[string]any{"kind": "extractive_quotes.v1", "claims": []any{map[string]any{"citation": citation, "quote": "Lease agreement."}}}
	result := map[string]any{"executionId": id, "agentId": "case-qa-agent", "agentVersion": "0.1.0", "status": "completed", "output": output, "evidence": []any{map[string]any{"documentId": c.DocumentId, "documentVersionId": c.VersionId, "passageId": c.PassageId, "contentSha256": c.ContentSha256}}, "proposedActions": []any{}, "requiresAttorneyReview": false}
	verify := func(wanted bool) {
		t.Helper()
		raw, _ := json.Marshal(result)
		err := f.s.tx(ctx, f.tenant, func(tx pgx.Tx) error {
			var verified bool
			if err := tx.QueryRow(ctx, "SELECT verify_execution_result($1,$2,$3::jsonb)", f.tenant, id, raw).Scan(&verified); err != nil {
				return err
			}
			if verified != wanted {
				t.Errorf("grounding verified=%v wanted=%v", verified, wanted)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	verify(true)
	evidence := result["evidence"].([]any)[0].(map[string]any)
	evidence["pageNumber"] = 99
	verify(false)
	delete(evidence, "pageNumber")
	output["kind"] = "unverified_free_prose"
	verify(false)
	output["kind"] = "extractive_quotes.v1"
	output["claims"] = []any{map[string]any{"citation": citation, "quote": "The court awarded damages."}}
	verify(false)
	output["claims"] = []any{map[string]any{"citation": citation, "quote": "Lease agreement."}}
	// This is the same transaction boundary used by workflow Store.Finish.
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", f.tenant)
	raw, _ := json.Marshal(result)
	var ok bool
	if err = tx.QueryRow(ctx, "SELECT verify_execution_result($1,$2,$3::jsonb)", f.tenant, id, raw).Scan(&ok); err != nil || !ok {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.IngestText(f.ctx(), &pb.IngestTextRequest{Scope: f.scope(), MatterId: f.matter, DocumentId: doc.Id, ExpectedRevision: 1, Title: "Revised", Text: "Lease changed.", IdempotencyKey: uuid.NewString()})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatal("source changed during verified save boundary", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	verify(false)
}
