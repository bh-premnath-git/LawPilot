package contextengine

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

func eventWorker(t *testing.T, f *fixture) *EventWorker {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("TEST_EVENT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	registry, err := LoadEvents("../../contracts/events/registry.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return &EventWorker{Pool: pool, Registry: registry, ID: uuid.NewString()}
}
func TestEventsPublicationInboxAndAuthorization(t *testing.T) {
	f := fixtureFor(t)
	w := eventWorker(t, f)
	f.ingest(t, "Lease terms.")
	for i := 0; i < 3; i++ {
		if err := w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var journal, inbox int
	f.admin.QueryRow(context.Background(), "SELECT count(*) FROM domain_event_journal WHERE tenant_id=$1", f.tenant).Scan(&journal)
	f.admin.QueryRow(context.Background(), "SELECT count(*) FROM domain_event_inbox WHERE tenant_id=$1", f.tenant).Scan(&inbox)
	if journal != 1 || inbox != 2 {
		t.Fatal("publication/fanout mismatch", journal, inbox)
	}
	events, err := f.s.ListMatterEvents(f.ctx(), &pb.ListMatterEventsRequest{Scope: f.scope(), MatterId: f.matter})
	if err != nil || len(events.GetEvents()) != 1 {
		t.Fatal(err)
	}
	// Redelivery after a lost consumer acknowledgement cannot duplicate effects.
	if _, err = f.admin.Exec(context.Background(), "UPDATE domain_event_deliveries SET state='pending' WHERE event_id IN(SELECT id FROM domain_event_journal WHERE tenant_id=$1)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if err = w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.admin.QueryRow(context.Background(), "SELECT count(*) FROM domain_event_inbox WHERE tenant_id=$1", f.tenant).Scan(&inbox)
	if inbox != 2 {
		t.Fatal("duplicate inbox effects")
	}
	f.admin.Exec(context.Background(), "UPDATE memberships SET status='revoked' WHERE tenant_id=$1", f.tenant)
	if _, err = f.s.ListMatterEvents(f.ctx(), &pb.ListMatterEventsRequest{Scope: f.scope(), MatterId: f.matter}); err == nil {
		t.Fatal("revoked feed access")
	}
}
func TestPublisherLeaseFencingAndUnknownSchemaDeadLetter(t *testing.T) {
	f := fixtureFor(t)
	w := eventWorker(t, f)
	f.ingest(t, "Lease.")
	ctx := context.Background()
	var id, token string
	if err := w.Pool.QueryRow(ctx, "SELECT event_id::text,lease_token::text FROM claim_domain_events($1,100,30) WHERE tenant_id=$2", w.ID, f.tenant).Scan(&id, &token); err != nil {
		t.Fatal(err)
	}
	f.admin.Exec(ctx, "UPDATE outbox_events SET lease_expires_at=now()-interval '1 second' WHERE id=$1", id)
	var newer string
	if err := w.Pool.QueryRow(ctx, "SELECT lease_token::text FROM claim_domain_events($1,100,30) WHERE event_id=$2", w.ID, id).Scan(&newer); err != nil {
		t.Fatal(err)
	}
	var settled bool
	if err := w.Pool.QueryRow(ctx, "SELECT publish_domain_event($1,$2,NULL)", id, token).Scan(&settled); err != nil || settled {
		t.Fatal("stale publisher settled", err)
	}
	if err := w.Pool.QueryRow(ctx, "SELECT publish_domain_event($1,$2,NULL)", id, newer).Scan(&settled); err != nil || !settled {
		t.Fatal(err)
	}
	badID := uuid.NewString()
	if _, err := f.admin.Exec(ctx, "INSERT INTO outbox_events(id,tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1,$2,'unknown.v1','1.0',$3,'{}')", badID, f.tenant, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	f.admin.QueryRow(ctx, "SELECT publish_state,last_error_code FROM outbox_events WHERE id=$1", badID).Scan(&state, &reason)
	if state != "dead_letter" || reason != "UNREGISTERED_EVENT" {
		t.Fatal(state, reason)
	}
	var replay bool
	if err := f.admin.QueryRow(ctx, "SELECT replay_domain_event($1)", badID).Scan(&replay); err != nil || !replay {
		t.Fatal("admin replay missing", err)
	}
}
func TestOutOfOrderRevisionsNeverRegressConsumerHeads(t *testing.T) {
	f := fixtureFor(t)
	w := eventWorker(t, f)
	doc := f.ingest(t, "Lease one.")
	if _, err := f.s.IngestText(f.ctx(), &pb.IngestTextRequest{Scope: f.scope(), MatterId: f.matter, DocumentId: doc.Id, ExpectedRevision: 1, Title: "Synthetic", Text: "Lease two.", IdempotencyKey: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows, err := w.Pool.Query(ctx, "SELECT event_id::text,lease_token::text,(event_body->'data'->>'revision')::bigint FROM claim_domain_events($1,100,30) WHERE tenant_id=$2 ORDER BY (event_body->'data'->>'revision')::bigint DESC", w.ID, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	type claim struct {
		id, token string
		rev       int64
	}
	items := []claim{}
	for rows.Next() {
		var c claim
		rows.Scan(&c.id, &c.token, &c.rev)
		items = append(items, c)
	}
	rows.Close()
	if len(items) != 2 {
		t.Fatal("missing revision events")
	}
	for _, c := range items {
		var ok bool
		if err = w.Pool.QueryRow(ctx, "SELECT publish_domain_event($1,$2,NULL)", c.id, c.token).Scan(&ok); err != nil || !ok {
			t.Fatal(err)
		}
	}
	// Process the newer revision before the older one even when delivery claims
	// have unrelated UUID ordering.
	for _, c := range items {
		var token string
		if err = w.Pool.QueryRow(ctx, "SELECT lease_token::text FROM claim_domain_deliveries(100,30) WHERE event_id=$1 AND consumer_id='aggregate-heads.v1'", c.id).Scan(&token); err != nil {
			// Other deliveries may already be leased by the prior batch; reclaim their
			// known lease token through the administrator solely in this synthetic test.
			if err = f.admin.QueryRow(ctx, "SELECT lease_token::text FROM domain_event_deliveries WHERE event_id=$1 AND consumer_id='aggregate-heads.v1'", c.id).Scan(&token); err != nil {
				t.Fatal(err)
			}
		}
		var ok bool
		if err = w.Pool.QueryRow(ctx, "SELECT consume_domain_delivery($1,'aggregate-heads.v1',$2)", c.id, token).Scan(&ok); err != nil || !ok {
			t.Fatal(err)
		}
	}
	var revision int64
	if err = f.admin.QueryRow(ctx, "SELECT revision FROM domain_aggregate_heads WHERE tenant_id=$1 AND consumer_id='aggregate-heads.v1' AND aggregate_id=$2", f.tenant, doc.Id).Scan(&revision); err != nil || revision != 2 {
		t.Fatal("head regressed", revision, err)
	}
	var stale int
	f.admin.QueryRow(ctx, "SELECT count(*) FROM domain_event_inbox WHERE tenant_id=$1 AND outcome='stale'", f.tenant).Scan(&stale)
	if stale != 1 {
		t.Fatal("out-of-order delivery not marked stale")
	}
}
func TestEventSchemaRejectsPrivilegedPayload(t *testing.T) {
	w := &EventWorker{Registry: map[string]bool{"document.version.created.v1": true}}
	tenant, id := uuid.NewString(), uuid.NewString()
	body := map[string]any{"specversion": "1.0", "id": uuid.NewString(), "type": "document.version.created.v1", "source": "context-engine", "tenantId": tenant, "aggregateId": id, "schemaVersion": "1.0", "time": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]any{"revision": 1, "documentText": "PRIVILEGED"}}
	raw, _ := json.Marshal(body)
	if code := w.Validate(tenant, "document.version.created.v1", "1.0", id, raw); code != "INVALID_EVENT_SCHEMA" {
		t.Fatal("privileged payload accepted")
	}
}

func TestEventRegistryMatchesDatabaseSchemaRegistry(t *testing.T) {
	f := fixtureFor(t)
	w := eventWorker(t, f)
	rows, err := f.admin.Query(context.Background(), "SELECT event_type FROM domain_event_types")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if !w.Registry[kind] {
			t.Fatal("database event missing from contract registry", kind)
		}
		count++
	}
	if rows.Err() != nil || count != len(w.Registry) {
		t.Fatal("event registry drift", rows.Err())
	}
}
