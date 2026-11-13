package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

type extensionFixture struct {
	s                                                   *Service
	admin                                               *pgxpool.Pool
	tenant, who, reviewer, matter, calendar, connection string
	server                                              *httptest.Server
	entries                                             []map[string]any
	posts, patches                                      int
	hookToken, hookID                                   string
	missing                                             bool
}

func extensionFor(t *testing.T) *extensionFixture {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("database disabled")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	pool, e := pgxpool.New(ctx, os.Getenv("TEST_INTEGRATION_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	f := &extensionFixture{admin: admin, tenant: uuid.NewString(), who: uuid.NewString(), reviewer: uuid.NewString(), matter: uuid.NewString(), calendar: uuid.NewString(), connection: uuid.NewString()}
	f.s = &Service{Pool: pool, Keys: &Keyring{Active: "test", Keys: map[string]string{"test": base64.StdEncoding.EncodeToString(make([]byte, 32))}}, Writes: true}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/webhooks.json":
			var body struct {
				Data struct {
					URL string `json:"url"`
				} `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			u, _ := url.Parse(body.Data.URL)
			parts := strings.Split(u.Path, "/")
			f.hookID = parts[len(parts)-2]
			f.hookToken = parts[len(parts)-1]
			if _, e := f.s.ReceiveWebhook(ctx, &pb.ReceiveWebhookRequest{SubscriptionId: f.hookID, CallbackToken: f.hookToken, Secret: "synthetic-webhook-secret-at-least32", Body: []byte(`{"meta":{"webhook_id":900}}`)}); e != nil {
				t.Error(e)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"data":{"id":900}}`)
		case r.URL.Path == "/webhooks/900/activate":
			if r.Header.Get("X-Hook-Secret") != "synthetic-webhook-secret-at-least32" {
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, `{}`)
		case r.URL.Path == "/calendar-entries" && r.Method == "POST":
			f.posts++
			var body struct {
				Data map[string]any `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			entry := body.Data
			entry["id"] = "500"
			entry["etag"] = "v1"
			f.entries = append(f.entries, entry)
			w.WriteHeader(201)
			io.WriteString(w, `{"data":{"id":500}}`)
		case r.URL.Path == "/calendar-entries/500" && r.Method == "PATCH":
			f.patches++
			if r.Header.Get("If-Match") != "v1" {
				w.WriteHeader(412)
				return
			}
			var body struct {
				Data map[string]any `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			entry := body.Data
			entry["id"] = "500"
			entry["etag"] = "v2"
			f.entries = []map[string]any{entry}
			io.WriteString(w, `{"data":{"id":500}}`)
		case r.URL.Path == "/calendar-entries/500":
			if f.missing {
				w.WriteHeader(404)
			} else {
				io.WriteString(w, `{"data":{"id":500}}`)
			}
		case r.URL.Path == "/calendar-entries":
			entries := []map[string]any{}
			if !f.missing {
				for _, entry := range f.entries {
					if marker := r.URL.Query().Get("external_property_value"); marker == "" || entry["operation_id"] == marker {
						entries = append(entries, entry)
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case r.URL.Path == "/matters":
			io.WriteString(w, `{"data":[{"id":1,"etag":"m1","title":"Matter","status":"active"}]}`)
		case r.URL.Path == "/calendars":
			io.WriteString(w, `{"data":[{"id":2,"etag":"c1","name":"Firm"}]}`)
		case r.URL.Path == "/matters/1" || r.URL.Path == "/calendars/2":
			io.WriteString(w, `{"data":{"id":1}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	p := &Provider{ID: "generic-rest", Base: f.server.URL, HTTP: f.server.Client(), TimeZone: "UTC"}
	clio := *p
	clio.ID = "clio-manage"
	f.s.Providers = map[string]*Provider{"generic-rest": p, "clio-manage": &clio}
	f.exec(t, "INSERT INTO tenants VALUES($1,'Synthetic extension')", f.tenant)
	for _, who := range []string{f.who, f.reviewer} {
		f.exec(t, "INSERT INTO memberships VALUES($1,$2,'active')", f.tenant, who)
		for _, cap := range []string{"provider.manage", "provider.sync", "provider.write", "provider.approve", "calendar.read", "calendar.write", "calendar.approve"} {
			f.exec(t, "INSERT INTO principal_capabilities VALUES($1,$2,$3)", f.tenant, who, cap)
		}
	}
	f.exec(t, "INSERT INTO matters(id,tenant_id,title,resource_owner)VALUES($1,$2,'Matter','provider:generic-rest')", f.matter, f.tenant)
	f.exec(t, "INSERT INTO calendars(id,tenant_id,name,default_time_zone,created_by,authority,overlap_policy)VALUES($1,$2,'Firm','UTC',$3,'provider:generic-rest','shared')", f.calendar, f.tenant, f.who)
	for _, who := range []string{f.who, f.reviewer} {
		f.exec(t, "INSERT INTO matter_grants VALUES($1,$2,$3,'matter.read')", f.tenant, f.matter, who)
		for _, perm := range []string{"read", "write", "approve"} {
			f.exec(t, "INSERT INTO calendar_grants VALUES($1,$2,$3,$4)", f.tenant, f.calendar, who, perm)
		}
	}
	f.addConnection(t, f.connection, "generic-rest")
	for _, v := range []struct{ Kind, External, Local, ETag string }{{"matter", "1", f.matter, "m1"}, {"calendar", "2", f.calendar, "c1"}} {
		f.exec(t, "INSERT INTO provider_sync_bindings(tenant_id,connection_id,resource_type,external_id,local_id,external_etag,snapshot_sha256)VALUES($1,$2,$3,$4,$5,$6,$7)", f.tenant, f.connection, v.Kind, v.External, v.Local, v.ETag, sha([]byte("fixture")))
	}
	t.Cleanup(func() {
		f.server.Close()
		for _, table := range []string{"provider_webhook_receipts", "provider_webhooks", "provider_sync_signals", "provider_calendar_operations", "integration_commands", "provider_sync_bindings", "provider_connection_grants", "integration_jobs", "provider_connections", "outbox_events", "audit_events", "calendar_entries", "calendar_grants", "calendars", "matter_grants", "matters", "principal_capabilities", "memberships"} {
			if _, e := admin.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id=$1", f.tenant); e != nil {
				t.Error(e)
			}
		}
		admin.Exec(ctx, "DELETE FROM tenants WHERE id=$1", f.tenant)
		pool.Close()
		admin.Close()
	})
	return f
}
func (f *extensionFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, e := f.admin.Exec(context.Background(), q, args...); e != nil {
		t.Fatal(e)
	}
}
func (f *extensionFixture) addConnection(t *testing.T, id, provider string) {
	tokens, _ := json.Marshal(Tokens{Access: "synthetic", Refresh: "synthetic", Type: "Bearer", ExpiresIn: 3600})
	encrypted, _ := f.s.Keys.Seal(tokens, f.tenant+"|"+id+"|"+provider)
	f.exec(t, "INSERT INTO provider_connections(id,tenant_id,provider_id,status,created_by,encrypted_tokens,token_expires_at,capabilities)VALUES($1,$2,$3,'active',$4,$5,now()+interval '1 hour','{}')", id, f.tenant, provider, f.who, encrypted)
	for _, who := range []string{f.who, f.reviewer} {
		for _, perm := range []string{"manage", "sync", "approve"} {
			f.exec(t, "INSERT INTO provider_connection_grants VALUES($1,$2,$3,$4)", f.tenant, id, who, perm)
		}
	}
}
func (f *extensionFixture) actor(who string) context.Context {
	return context.WithValue(context.Background(), principalKey{}, who)
}
func (f *extensionFixture) sync(t *testing.T) {
	t.Helper()
	j, e := f.s.RequestSync(f.actor(f.who), &pb.SyncRequest{Scope: &pb.Scope{TenantId: f.tenant}, ConnectionId: f.connection, IdempotencyKey: uuid.NewString()})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 20; i++ {
		result, e := f.s.RunJob(context.Background(), &pb.Ref{TenantId: f.tenant, Id: j.Id})
		if e != nil {
			t.Fatal(e)
		}
		if result.State == "completed" {
			return
		}
		if result.State == "failed" {
			t.Fatal(result)
		}
	}
	t.Fatal("sync failed to terminate")
}
func TestApprovedAppointmentsAndRemoteDeletionReconciliation(t *testing.T) {
	t.Setenv("PROVIDER_CALENDAR_WRITES_ENABLED", "true")
	f := extensionFor(t)
	scope := &pb.Scope{TenantId: f.tenant}
	r := &pb.CalendarWriteRequest{Scope: scope, ConnectionId: f.connection, CalendarId: f.calendar, MatterId: f.matter, Operation: "create", Title: "Reviewed appointment", StartAt: "2026-12-01T10:00:00Z", EndAt: "2026-12-01T11:00:00Z", TimeZone: "UTC", IdempotencyKey: uuid.NewString()}
	p, e := f.s.ProposeCalendarWrite(f.actor(f.who), r)
	if e != nil {
		t.Fatal(e)
	}
	d := &pb.CalendarOperationRequest{Scope: scope, OperationId: p.OperationId, ArtifactSha256: p.ArtifactSha256, Decision: "approve", IdempotencyKey: uuid.NewString()}
	if _, e = f.s.DecideCalendarWrite(f.actor(f.who), d); e == nil {
		t.Fatal("self approval")
	}
	if _, e = f.s.DecideCalendarWrite(f.actor(f.reviewer), d); e != nil {
		t.Fatal(e)
	}
	result, e := f.s.RunJob(context.Background(), &pb.Ref{TenantId: f.tenant, Id: p.OperationId})
	if e != nil || result.State != "completed" {
		t.Fatal(result, e)
	}
	f.s.RunJob(context.Background(), &pb.Ref{TenantId: f.tenant, Id: p.OperationId})
	if f.posts != 1 {
		t.Fatal("duplicate creation")
	}
	f.sync(t)
	var entry string
	if e = f.admin.QueryRow(context.Background(), "SELECT local_id::text FROM provider_sync_bindings WHERE tenant_id=$1 AND resource_type='calendar_entry'", f.tenant).Scan(&entry); e != nil {
		t.Fatal(e)
	}
	r.Operation = "update"
	r.EntryId = entry
	f.exec(t, "UPDATE provider_sync_bindings SET remote_recurrence='FREQ=WEEKLY' WHERE tenant_id=$1 AND local_id=$2", f.tenant, entry)
	if _, e = f.s.ProposeCalendarWrite(f.actor(f.who), r); e == nil {
		t.Fatal("recurring appointment write allowed")
	}
	f.exec(t, "UPDATE provider_sync_bindings SET remote_recurrence='' WHERE tenant_id=$1 AND local_id=$2", f.tenant, entry)
	r.EntryId = entry
	r.Title = "Reviewed edit"
	r.IdempotencyKey = uuid.NewString()
	p, e = f.s.ProposeCalendarWrite(f.actor(f.who), r)
	if e != nil {
		t.Fatal(e)
	}
	d.OperationId = p.OperationId
	d.ArtifactSha256 = p.ArtifactSha256
	d.IdempotencyKey = uuid.NewString()
	if _, e = f.s.DecideCalendarWrite(f.actor(f.reviewer), d); e != nil {
		t.Fatal(e)
	}
	result, e = f.s.RunJob(context.Background(), &pb.Ref{TenantId: f.tenant, Id: p.OperationId})
	if e != nil || result.State != "completed" || f.patches != 1 {
		t.Fatal(result, e)
	}
	f.sync(t)
	f.missing = true
	f.sync(t)
	var deleted bool
	var state string
	f.admin.QueryRow(context.Background(), "SELECT deleted FROM provider_sync_bindings WHERE tenant_id=$1 AND local_id=$2", f.tenant, entry).Scan(&deleted)
	f.admin.QueryRow(context.Background(), "SELECT status FROM calendar_entries WHERE id=$1", entry).Scan(&state)
	if !deleted || state != "cancelled" {
		t.Fatal("missing remote appointment remained active")
	}
}
func TestWebhookHandshakeSignatureReplayAndDurableSignals(t *testing.T) {
	t.Setenv("PROVIDER_WEBHOOKS_ENABLED", "true")
	t.Setenv("APP_ENV", "development")
	f := extensionFor(t)
	t.Setenv("PROVIDER_WEBHOOK_PUBLIC_URL", "http://callback.invalid/v1/provider-webhooks")
	conn := uuid.NewString()
	f.addConnection(t, conn, "clio-manage")
	r := &pb.WebhookRequest{Scope: &pb.Scope{TenantId: f.tenant}, ConnectionId: conn, Model: "calendar_entry", IdempotencyKey: uuid.NewString()}
	hook, e := f.s.RegisterWebhook(f.actor(f.who), r)
	if e != nil || hook.State != "pending" {
		t.Fatal(hook, e)
	}
	r.SubscriptionId = hook.SubscriptionId
	if _, e = f.s.ActivateWebhook(f.actor(f.who), r); e != nil {
		t.Fatal(e)
	}
	body := []byte(`{"data":{"id":500},"meta":{"event":"updated","webhook_id":900}}`)
	mac := hmac.New(sha256.New, []byte("synthetic-webhook-secret-at-least32"))
	mac.Write(body)
	request := &pb.ReceiveWebhookRequest{SubscriptionId: hook.SubscriptionId, CallbackToken: f.hookToken, Body: body, Signature: hex.EncodeToString(mac.Sum(nil))}
	if _, e = f.s.ReceiveWebhook(context.Background(), request); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.ReceiveWebhook(context.Background(), request); e != nil {
		t.Fatal(e)
	}
	var generation int64
	f.admin.QueryRow(context.Background(), "SELECT generation FROM provider_sync_signals WHERE connection_id=$1", conn).Scan(&generation)
	if generation != 1 {
		t.Fatal("replay queued extra work")
	}
	request.Body = append(request.Body, ' ')
	if _, e = f.s.ReceiveWebhook(context.Background(), request); e == nil {
		t.Fatal("tampered signature accepted")
	}
	if e = f.s.DrainSyncSignals(context.Background()); e != nil {
		t.Fatal(e)
	}
	var count int
	f.admin.QueryRow(context.Background(), "SELECT count(*) FROM integration_jobs WHERE tenant_id=$1 AND resource_id=$2", f.tenant, conn).Scan(&count)
	if count != 1 {
		t.Fatal("notification not durably queued", count)
	}
	if _, e = f.s.Disconnect(f.actor(f.who), &pb.ConnectionRequest{Scope: &pb.Scope{TenantId: f.tenant}, ConnectionId: conn}); e != nil {
		t.Fatal(e)
	}
	request.Body = body
	if _, e = f.s.ReceiveWebhook(context.Background(), request); e == nil {
		t.Fatal("disconnected webhook remained active")
	}
	var destroyed bool
	f.admin.QueryRow(context.Background(), "SELECT encrypted_secret IS NULL FROM provider_webhooks WHERE id=$1", hook.SubscriptionId).Scan(&destroyed)
	if !destroyed {
		t.Fatal("signing secret retained after disconnect")
	}

}
