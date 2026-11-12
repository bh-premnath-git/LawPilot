package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) ListCalendars(ctx context.Context, r *pb.ListCalendarsRequest) (*pb.ListCalendarsResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.ListCalendarsResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, "", false, func(tx pgx.Tx) error {
		if err := member(ctx, tx, tenant, who); err != nil {
			return err
		}
		staff, err := capability(ctx, tx, tenant, who, "calendar.read")
		if err != nil {
			return err
		}
		client, err := capability(ctx, tx, tenant, who, "calendar.client_read")
		if err != nil {
			return err
		}
		if !staff && !client {
			return status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
		}
		rows, err := tx.Query(ctx, "SELECT c.id::text,c.name,c.default_time_zone,c.approval_required,c.revision,c.authority FROM calendars c WHERE c.tenant_id=$1 AND EXISTS(SELECT 1 FROM calendar_grants g WHERE g.tenant_id=c.tenant_id AND g.calendar_id=c.id AND g.principal_id=$2 AND g.permission='read') ORDER BY c.id LIMIT 500", tenant, who)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value := new(pb.Calendar)
			if err = rows.Scan(&value.Id, &value.Name, &value.DefaultTimeZone, &value.ApprovalRequired, &value.Revision, &value.Authority); err != nil {
				return err
			}
			reply.Calendars = append(reply.Calendars, value)
		}
		return rows.Err()
	})
	return reply, err
}
func (s *Service) GetEntry(ctx context.Context, r *pb.GetEntryRequest) (*pb.GetEntryResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.GetEntryResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, false, func(tx pgx.Tx) error {
		_, client, err := authorize(ctx, tx, tenant, r.CalendarId, who, "read")
		if err != nil {
			return err
		}
		reply.Entry, _, err = loadEntry(ctx, tx, tenant, r.CalendarId, r.EntryId)
		if err != nil {
			return err
		}
		if !visible(ctx, tx, tenant, who, reply.Entry, client) {
			return status.Error(codes.NotFound, "NOT_FOUND")
		}
		redact(reply.Entry, who, client)
		return nil
	})
	return reply, err
}
func bounds(from, to *timestamppb.Timestamp) (time.Time, time.Time, error) {
	if from == nil || to == nil || from.CheckValid() != nil || to.CheckValid() != nil {
		return time.Time{}, time.Time{}, bad("EXPLICIT_INSTANTS_REQUIRED")
	}
	a, b := from.AsTime(), to.AsTime()
	if !b.After(a) || b.Sub(a) > 93*24*time.Hour {
		return a, b, bad("RANGE_MUST_BE_AT_MOST_93_DAYS")
	}
	return a, b, nil
}

type cursor struct {
	Start  time.Time `json:"start"`
	ID     string    `json:"id"`
	Filter string    `json:"filter"`
}

func (s *Service) ListEntries(ctx context.Context, r *pb.ListEntriesRequest) (*pb.ListEntriesResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	from, to, err := bounds(r.From, r.To)
	if err != nil {
		return nil, err
	}
	if r.MatterId != "" && !identifier(r.MatterId) {
		return nil, bad("INVALID_MATTER_ID")
	}
	if r.PageSize < 0 || r.PageSize > 100 {
		return nil, bad("INVALID_PAGE_SIZE")
	}
	size := int(r.PageSize)
	if size == 0 {
		size = 50
	}
	reply := &pb.ListEntriesResponse{Coverage: "native_only", AsOf: timestamppb.New(s.now())}
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, false, func(tx pgx.Tx) error {
		_, client, err := authorize(ctx, tx, tenant, r.CalendarId, who, "read")
		if err != nil {
			return err
		}
		if err = matterAccess(ctx, tx, tenant, who, r.MatterId); err != nil {
			return err
		}
		filter := sha256.Sum256([]byte(tenant + "|" + r.CalendarId + "|" + who + "|" + from.Format(time.RFC3339Nano) + "|" + to.Format(time.RFC3339Nano) + "|" + r.MatterId))
		digest := hex.EncodeToString(filter[:])
		var after *time.Time
		afterID := "00000000-0000-0000-0000-000000000000"
		if r.PageToken != "" {
			if len(r.PageToken) > 512 {
				return bad("INVALID_PAGE_TOKEN")
			}
			body, err := base64.RawURLEncoding.DecodeString(r.PageToken)
			if err != nil {
				return bad("INVALID_PAGE_TOKEN")
			}
			var token cursor
			if json.Unmarshal(body, &token) != nil || token.Filter != digest || !identifier(token.ID) || token.Start.IsZero() {
				return bad("INVALID_PAGE_TOKEN")
			}
			after = &token.Start
			afterID = token.ID
		}
		rows, err := tx.Query(ctx, "SELECT "+columns+join+`WHERE e.tenant_id=$1 AND e.calendar_id=$2 AND e.status='active' AND e.start_at<$6 AND e.end_at>$5
   AND (e.visibility<>1 OR e.created_by=$3) AND (NOT $4::boolean OR (e.visibility=3 AND e.kind IN(1,2) AND $3=ANY(e.participants)))
   AND (e.matter_id IS NULL OR EXISTS(SELECT 1 FROM matter_grants g WHERE g.tenant_id=e.tenant_id AND g.matter_id=e.matter_id AND g.principal_id=$3 AND g.permission='matter.read'))
   AND ($7::text='' OR e.matter_id=NULLIF($7,'')::uuid) AND ($8::timestamptz IS NULL OR (e.start_at,e.id)>($8::timestamptz,$9::uuid))
   ORDER BY e.start_at,e.id LIMIT $10`, tenant, r.CalendarId, who, client, from, to, r.MatterId, after, afterID, size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var last cursor
		for rows.Next() {
			entry, when, err := scan(rows)
			if err != nil {
				return err
			}
			if len(reply.Entries) == size {
				body, _ := json.Marshal(last)
				reply.NextPageToken = base64.RawURLEncoding.EncodeToString(body)
				break
			}
			redact(entry, who, client)
			reply.Entries = append(reply.Entries, entry)
			last = cursor{Start: when.Start, ID: entry.Id, Filter: digest}
		}
		return rows.Err()
	})
	return reply, err
}
func (s *Service) Availability(ctx context.Context, r *pb.AvailabilityRequest) (*pb.AvailabilityResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	from, to, err := bounds(r.From, r.To)
	if err != nil {
		return nil, err
	}
	reply := &pb.AvailabilityResponse{Coverage: "native_only", Warnings: []string{"External calendars are not connected; external availability is unknown."}}
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, false, func(tx pgx.Tx) error {
		_, client, err := authorize(ctx, tx, tenant, r.CalendarId, who, "read")
		if err != nil {
			return err
		}
		allowed, err := capability(ctx, tx, tenant, who, "calendar.availability")
		if err != nil {
			return err
		}
		if client || !allowed {
			return status.Error(codes.PermissionDenied, "BUSY_ACCESS_DENIED")
		}
		rows, err := tx.Query(ctx, "SELECT GREATEST(start_at,$3),LEAST(end_at,$4) FROM calendar_entries WHERE tenant_id=$1 AND calendar_id=$2 AND status='active' AND kind<>5 AND start_at<$4 AND end_at>$3 ORDER BY start_at LIMIT 1001", tenant, r.CalendarId, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a, b time.Time
			if err = rows.Scan(&a, &b); err != nil {
				return err
			}
			if len(reply.Busy) >= 1000 {
				return status.Error(codes.ResourceExhausted, "NARROW_AVAILABILITY_RANGE")
			}
			reply.Busy = append(reply.Busy, &pb.BusyInterval{Start: timestamppb.New(a), End: timestamppb.New(b)})
		}
		return rows.Err()
	})
	return reply, err
}
func (s *Service) SyncStatus(ctx context.Context, r *pb.SyncStatusRequest) (*pb.SyncStatusResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := &pb.SyncStatusResponse{Authority: "lawpilot-native", State: "native_only", Coverage: "native_only", Warnings: []string{"No Clio or external calendar connection is implemented."}}
	err = s.tx(ctx, tenant, r.CalendarId, false, func(tx pgx.Tx) error {
		_, _, err := authorize(ctx, tx, tenant, r.CalendarId, actor(ctx), "read")
		return err
	})
	return reply, err
}
