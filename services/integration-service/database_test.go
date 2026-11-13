package integrations

import (
	"context"
	"encoding/base64"
	"fmt"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
)

func TestOAuthSyncAndDistinctApprovedUpdate(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("database integration disabled")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_INTEGRATION_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tenant, who, reviewer := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO tenants VALUES($1,'Synthetic integrations')", tenant)
	t.Cleanup(func() {
		for _, table := range []string{"integration_commands", "provider_oauth_states", "provider_update_proposals", "provider_sync_bindings", "provider_connection_grants", "integration_jobs", "provider_connections", "outbox_events", "audit_events", "calendar_entries", "calendar_grants", "calendars", "matter_grants", "matters", "principal_capabilities", "memberships"} {
			if _, e := admin.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id=$1", tenant); e != nil {
				t.Error(e)
			}
		}
		admin.Exec(ctx, "DELETE FROM tenants WHERE id=$1", tenant)
	})
	for _, actor := range []string{who, reviewer} {
		exec("INSERT INTO memberships VALUES($1,$2,'active')", tenant, actor)
		for _, cap := range []string{"provider.manage", "provider.sync", "provider.read", "provider.write", "provider.approve"} {
			exec("INSERT INTO principal_capabilities VALUES($1,$2,$3)", tenant, actor, cap)
		}
	}
	title, etag, patches, exchanges := "Original", "v1", 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/token":
			exchanges++
			io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600}`)
		case r.Method == "PATCH":
			patches++
			if r.Header.Get("If-Match") != etag {
				w.WriteHeader(412)
				return
			}
			title = "Approved title"
			etag = "v2"
			io.WriteString(w, `{"data":{}}`)
		case r.URL.Path == "/matters/1":
			fmt.Fprintf(w, `{"data":{"id":1,"etag":%q,"title":%q}}`, etag, title)
		case r.URL.Path == "/matters":
			fmt.Fprintf(w, `{"data":[{"id":1,"etag":%q,"title":%q,"status":"active"}]}`, etag, title)
		case r.URL.Path == "/calendars":
			io.WriteString(w, `{"data":[{"id":2,"etag":"c1","name":"Shared firm"}]}`)
		case r.URL.Path == "/calendar-entries":
			io.WriteString(w, `{"data":[{"id":3,"etag":"e1","calendar_id":2,"matter_id":1,"title":"First attorney","start_at":"2026-11-20T10:00:00Z","end_at":"2026-11-20T11:00:00Z","time_zone":"UTC"},{"id":4,"etag":"e2","calendar_id":2,"matter_id":1,"title":"Second attorney","start_at":"2026-11-20T10:00:00Z","end_at":"2026-11-20T11:00:00Z","time_zone":"UTC"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	s := &Service{Pool: pool, Providers: map[string]*Provider{"generic-rest": {ID: "generic-rest", Base: server.URL, AuthorizeURL: server.URL + "/authorize", TokenURL: server.URL + "/token", ClientID: "client", ClientSecret: "secret", Redirect: "https://local.invalid/callback", HTTP: server.Client(), TimeZone: "UTC"}}, Keys: &Keyring{Active: "one", Keys: map[string]string{"one": base64.StdEncoding.EncodeToString(make([]byte, 32))}}, Writes: true}
	actorCtx := context.WithValue(ctx, principalKey{}, who)
	reviewCtx := context.WithValue(ctx, principalKey{}, reviewer)
	scope := &pb.Scope{TenantId: tenant}
	created, err := s.CreateConnection(actorCtx, &pb.CreateConnectionRequest{Scope: scope, ProviderId: "generic-rest", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	conn := created.Connection.Id
	auth, err := s.AuthorizeConnection(actorCtx, &pb.ConnectionRequest{Scope: scope, ConnectionId: conn})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.AuthorizeUrl)
	complete := &pb.OAuthCompleteRequest{Scope: scope, ConnectionId: conn, State: u.Query().Get("state"), Code: "synthetic-code"}
	if _, err = s.CompleteOAuth(actorCtx, complete); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteOAuth(actorCtx, complete); err == nil || exchanges != 1 {
		t.Fatal("OAuth replay reached provider")
	}
	var cipher []byte
	if err = admin.QueryRow(ctx, "SELECT encrypted_tokens FROM provider_connections WHERE id=$1", conn).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if string(cipher) == "synthetic-access" {
		t.Fatal("plaintext token")
	}
	sync, err := s.RequestSync(actorCtx, &pb.SyncRequest{Scope: scope, ConnectionId: conn, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		job, err := s.RunJob(ctx, &pb.Ref{TenantId: tenant, Id: sync.Id})
		if err != nil || job.State == "failed" {
			t.Fatal("sync", job, err)
		}
	}
	var matter string
	var count int
	admin.QueryRow(ctx, "SELECT local_id::text FROM provider_sync_bindings WHERE tenant_id=$1 AND resource_type='matter'", tenant).Scan(&matter)
	admin.QueryRow(ctx, "SELECT count(*) FROM calendar_entries WHERE tenant_id=$1", tenant).Scan(&count)
	if count != 2 {
		t.Fatal("shared provider calendar rejected overlap", count)
	}
	for _, actor := range []string{who, reviewer} {
		exec("INSERT INTO matter_grants VALUES($1,$2,$3,'matter.read')", tenant, matter, actor)
	}
	// Grant a second human explicit connection approval; global capability alone is insufficient.
	exec("INSERT INTO provider_connection_grants VALUES($1,$2,$3,'approve')", tenant, conn, reviewer)
	proposal, err := s.ProposeProviderUpdate(actorCtx, &pb.ProposeProviderUpdateRequest{Scope: scope, ConnectionId: conn, MatterId: matter, Title: "Approved title", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	decision := &pb.DecideProviderUpdateRequest{Scope: scope, OperationId: proposal.OperationId, ArtifactSha256: proposal.ArtifactSha256, Decision: "approve", IdempotencyKey: uuid.NewString()}
	if _, err = s.DecideProviderUpdate(actorCtx, decision); err == nil {
		t.Fatal("self approval accepted")
	}
	if _, err = s.DecideProviderUpdate(reviewCtx, decision); err != nil {
		t.Fatal(err)
	}
	result, err := s.RunJob(ctx, &pb.Ref{TenantId: tenant, Id: proposal.OperationId})
	if err != nil || result.State != "completed" {
		t.Fatal("approved provider write", result, err)
	}
	if _, err = s.RunJob(ctx, &pb.Ref{TenantId: tenant, Id: proposal.OperationId}); err != nil || patches != 1 {
		t.Fatal("duplicate external write", err, patches)
	}
	// Revoked synchronization cannot reach provider updates.
	exec("DELETE FROM provider_connection_grants WHERE tenant_id=$1 AND connection_id=$2 AND principal_id=$3 AND permission='sync'", tenant, conn, who)
	if _, err = s.RequestSync(actorCtx, &pb.SyncRequest{Scope: scope, ConnectionId: conn, IdempotencyKey: uuid.NewString()}); err == nil {
		t.Fatal("revoked sync allowed")
	}
}
