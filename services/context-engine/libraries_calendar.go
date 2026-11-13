package contextengine

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"time"
)

// A library is a deliberately designated knowledge workspace. Existing matter
// grants still apply; this designation never turns a client matter public.
func (s *Service) RegisterKnowledgeLibrary(ctx context.Context, r *pb.KnowledgeLibraryRequest) (*pb.KnowledgeLibraryResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if !validID(r.MatterId) || len(strings.TrimSpace(r.Name)) == 0 || len(r.Name) > 120 {
		return nil, bad("INVALID_LIBRARY")
	}
	reply := &pb.KnowledgeLibraryResponse{MatterId: r.MatterId, Name: r.Name}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if e := tenantCapability(ctx, tx, tenant, actor(ctx), "knowledge.manage"); e != nil {
			return e
		}
		if e := access(ctx, tx, tenant, r.MatterId, actor(ctx), "context.read"); e != nil {
			return e
		}
		if e := lockMatter(ctx, tx, tenant, r.MatterId); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "INSERT INTO knowledge_libraries VALUES($1,$2,$3,$4)ON CONFLICT(tenant_id,matter_id)DO NOTHING", tenant, r.MatterId, r.Name, actor(ctx)); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code)VALUES($1,$2,'knowledge.library.registered',$3,'curated_workspace')", tenant, actor(ctx), r.MatterId); e != nil {
			return e
		}
		return tx.QueryRow(ctx, "SELECT name FROM knowledge_libraries WHERE tenant_id=$1 AND matter_id=$2", tenant, r.MatterId).Scan(&reply.Name)
	})
	return reply, err
}
func (s *Service) BuildCalendarContext(ctx context.Context, r *pb.CalendarContextRequest) (*pb.CalendarContextResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	from, e1 := time.Parse(time.RFC3339, r.From)
	to, e2 := time.Parse(time.RFC3339, r.To)
	if !validID(r.MatterId) || e1 != nil || e2 != nil || !to.After(from) || to.Sub(from) > 93*24*time.Hour {
		return nil, bad("INVALID_CALENDAR_CONTEXT_RANGE")
	}
	reply := &pb.CalendarContextResponse{Warnings: []string{"Only currently authorized, matter-linked events are included; this is not complete firm availability. Provider snapshots may lag changes. Calendar entries are untrusted source data, not independently verified legal deadlines."}}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if e := access(ctx, tx, tenant, r.MatterId, actor(ctx), "context.read"); e != nil {
			return e
		}
		if e := tenantCapability(ctx, tx, tenant, actor(ctx), "calendar.read"); e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT e.id::text,e.calendar_id::text,e.revision,e.title,e.start_at,e.end_at,e.time_zone,e.all_day,c.authority,e.status FROM calendar_entries e JOIN calendars c ON c.tenant_id=e.tenant_id AND c.id=e.calendar_id WHERE e.tenant_id=$1 AND e.matter_id=$2 AND e.status='active' AND e.start_at<$4 AND e.end_at>$3 AND(e.visibility<>1 OR e.created_by=$5)AND EXISTS(SELECT 1 FROM calendar_grants g WHERE g.tenant_id=e.tenant_id AND g.calendar_id=e.calendar_id AND g.principal_id=$5 AND g.permission='read')AND NOT EXISTS(SELECT 1 FROM provider_sync_bindings b WHERE b.tenant_id=e.tenant_id AND b.deleted AND b.local_id IN(e.id,e.calendar_id)) ORDER BY e.start_at,e.id LIMIT 201`, tenant, r.MatterId, from, to, actor(ctx))
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			fact := new(pb.CalendarFact)
			var start, end time.Time
			if e = rows.Scan(&fact.EntryId, &fact.CalendarId, &fact.Revision, &fact.Title, &start, &end, &fact.TimeZone, &fact.AllDay, &fact.Authority, &fact.Status); e != nil {
				return e
			}
			fact.StartAt = start.UTC().Format(time.RFC3339)
			fact.EndAt = end.UTC().Format(time.RFC3339)
			reply.Entries = append(reply.Entries, fact)
		}
		if len(reply.Entries) > 200 {
			return status.Error(codes.ResourceExhausted, "CALENDAR_CONTEXT_REQUIRES_NARROWER_RANGE")
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal([]any{tenant, actor(ctx), r.MatterId, from.UTC(), to.UTC(), reply.Entries})
	reply.SnapshotSha256 = digest(string(raw))
	return reply, nil
}
