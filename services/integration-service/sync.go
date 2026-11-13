package integrations

import (
	"context"
	"encoding/json"
	"errors"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/url"
	"time"
)

type syncCursor struct {
	Kind         string `json:"kind"`
	Next         string `json:"next"`
	Pages        int    `json:"pages"`
	CalendarFrom string `json:"calendarFrom"`
	CalendarTo   string `json:"calendarTo"`
}

func binding(ctx context.Context, tx pgx.Tx, tenant, connection, kind, external string) (string, string, error) {
	var id, etag string
	err := tx.QueryRow(ctx, "SELECT local_id::text,CASE WHEN deleted THEN '' ELSE external_etag END FROM provider_sync_bindings WHERE tenant_id=$1 AND connection_id=$2 AND resource_type=$3 AND external_id=$4", tenant, connection, kind, external).Scan(&id, &etag)
	return id, etag, err
}
func remember(ctx context.Context, tx pgx.Tx, tenant, connection, kind, external, id, etag string, value any, jobID string) error {
	raw, _ := json.Marshal(value)
	_, err := tx.Exec(ctx, "INSERT INTO provider_sync_bindings(tenant_id,connection_id,resource_type,external_id,local_id,external_etag,snapshot_sha256,seen_job_id)VALUES($1,$2,$3,$4,$5,$6,$7,$8)ON CONFLICT(tenant_id,connection_id,resource_type,external_id)DO UPDATE SET external_etag=excluded.external_etag,snapshot_sha256=excluded.snapshot_sha256,last_synced_at=now(),seen_job_id=excluded.seen_job_id,deleted=false", tenant, connection, kind, external, id, etag, sha(raw), jobID)
	if err == nil && kind == "calendar_entry" {
		if entry, ok := value.(RemoteEntry); ok {
			_, err = tx.Exec(ctx, "UPDATE provider_sync_bindings SET remote_recurrence=$5,remote_court_rule=$6 WHERE tenant_id=$1 AND connection_id=$2 AND resource_type=$3 AND external_id=$4", tenant, connection, kind, external, entry.Recurrence, entry.CourtRule)
		}
	}
	return err
}
func (s *Service) processSync(ctx context.Context, tenant, jobID, connectionID, who string) (*pb.JobResponse, error) {
	p, token, err := s.token(ctx, tenant, connectionID, who, "sync")
	if err != nil {
		return nil, err
	}
	cursor := syncCursor{Kind: "matter", CalendarFrom: time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339), CalendarTo: time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)}
	var raw []byte
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT cursor FROM integration_jobs WHERE tenant_id=$1 AND id=$2", tenant, jobID).Scan(&raw)
	})
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 && string(raw) != "{}" {
		if json.Unmarshal(raw, &cursor) != nil {
			return nil, status.Error(codes.DataLoss, "SYNC_CURSOR_CORRUPT")
		}
	}
	if cursor.Kind == "reconcile" {
		return s.reconcileMissing(ctx, tenant, jobID, connectionID, who, p, token)
	}
	if cursor.Pages >= 1000 {
		return nil, status.Error(codes.ResourceExhausted, "SYNC_PAGE_LIMIT")
	}
	fetchCursor := cursor.Next
	if cursor.Kind == "calendar_entry" && fetchCursor == "" && p.ID == "clio-manage" {
		initial, e := p.safeURL(cursor.Kind, "")
		if e != nil {
			return nil, e
		}
		u, _ := url.Parse(initial)
		q := u.Query()
		q.Set("expanded", "true")
		q.Set("from", cursor.CalendarFrom)
		q.Set("to", cursor.CalendarTo)
		u.RawQuery = q.Encode()
		fetchCursor = u.String()
	}
	page, err := p.Fetch(ctx, cursor.Kind, fetchCursor, token)
	if err != nil {
		return nil, unavailable("PROVIDER_SYNC_PAGE_UNAVAILABLE")
	}
	state := "running"
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, tenant, connectionID); err != nil {
			return err
		}
		c, err := connectionAccess(ctx, tx, tenant, connectionID, who, "sync")
		if err != nil {
			return err
		}
		if c.State != "active" {
			return status.Error(codes.FailedPrecondition, "CONNECTION_NOT_ACTIVE")
		}
		var current []byte
		if err = tx.QueryRow(ctx, "SELECT cursor FROM integration_jobs WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, jobID).Scan(&current); err != nil {
			return err
		}
		if string(current) != string(raw) {
			return status.Error(codes.Aborted, "SYNC_CURSOR_CHANGED")
		}
		for _, m := range page.Matters {
			id, old, e := binding(ctx, tx, tenant, connectionID, "matter", m.ID)
			if errors.Is(e, pgx.ErrNoRows) {
				id = uuid.NewString()
				_, e = tx.Exec(ctx, "INSERT INTO matters(id,tenant_id,title,status,resource_owner)VALUES($1,$2,$3,$4,$5)", id, tenant, m.Title, m.State, "provider:"+p.ID)
			} else if e == nil && old != m.ETag {
				_, e = tx.Exec(ctx, "UPDATE matters SET title=$3,status=$4,revision=revision+1 WHERE tenant_id=$1 AND id=$2 AND resource_owner=$5", tenant, id, m.Title, m.State, "provider:"+p.ID)
			}
			if e != nil {
				return e
			}
			if old != m.ETag {
				if e = changed(ctx, tx, tenant, id, "matters", "matter.updated.v1"); e != nil {
					return e
				}
			}
			if e = remember(ctx, tx, tenant, connectionID, "matter", m.ID, id, m.ETag, m, jobID); e != nil {
				return e
			}
		}
		for _, cal := range page.Calendars {
			id, old, e := binding(ctx, tx, tenant, connectionID, "calendar", cal.ID)
			if errors.Is(e, pgx.ErrNoRows) {
				id = uuid.NewString()
				_, e = tx.Exec(ctx, "INSERT INTO calendars(id,tenant_id,name,default_time_zone,approval_required,created_by,authority,overlap_policy)VALUES($1,$2,$3,$6,false,$4,$5,'shared')", id, tenant, cal.Name, c.Owner, "provider:"+p.ID, p.TimeZone)
				if e == nil {
					for _, permission := range []string{"read", "manage"} {
						if _, e = tx.Exec(ctx, "INSERT INTO calendar_grants VALUES($1,$2,$3,$4)", tenant, id, c.Owner, permission); e != nil {
							return e
						}
					}
				}
			} else if e == nil && old != cal.ETag {
				_, e = tx.Exec(ctx, "UPDATE calendars SET name=$3,revision=revision+1 WHERE tenant_id=$1 AND id=$2 AND authority=$4", tenant, id, cal.Name, "provider:"+p.ID)
			}
			if e != nil {
				return e
			}
			if old != cal.ETag {
				if e = changed(ctx, tx, tenant, id, "calendars", "calendar.created.v1"); e != nil {
					return e
				}
			}
			if e = remember(ctx, tx, tenant, connectionID, "calendar", cal.ID, id, cal.ETag, cal, jobID); e != nil {
				return e
			}
		}
		for _, event := range page.Entries {
			calendar, _, e := binding(ctx, tx, tenant, connectionID, "calendar", event.Calendar)
			if e != nil {
				return status.Error(codes.FailedPrecondition, "PROVIDER_CALENDAR_NOT_MAPPED")
			}
			var matter any
			if event.Matter != "" {
				mapped, _, e := binding(ctx, tx, tenant, connectionID, "matter", event.Matter)
				if e != nil {
					return status.Error(codes.FailedPrecondition, "PROVIDER_MATTER_NOT_MAPPED")
				}
				matter = mapped
			}
			var startDate, endDate any
			if event.AllDay {
				loc, _ := time.LoadLocation(event.Zone)
				a, b := event.Start.In(loc), event.End.In(loc)
				if a.Hour() != 0 || a.Minute() != 0 || a.Second() != 0 || b.Hour() != 0 || b.Minute() != 0 || b.Second() != 0 {
					return status.Error(codes.FailedPrecondition, "PROVIDER_ALL_DAY_ZONE_MISMATCH")
				}
				startDate = time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
				endDate = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
			}
			id, old, e := binding(ctx, tx, tenant, connectionID, "calendar_entry", event.ID)
			if errors.Is(e, pgx.ErrNoRows) {
				id = uuid.NewString()
				_, e = tx.Exec(ctx, "INSERT INTO calendar_entries(id,tenant_id,calendar_id,title,description,kind,visibility,matter_id,start_at,end_at,time_zone,all_day,start_date,end_date,participants,created_by,overlap_policy)VALUES($1,$2,$3,$4,$5,1,1,$6,$7,$8,$9,$10,$11,$12,'{}',$13,'shared')", id, tenant, calendar, event.Title, event.Description, matter, event.Start, event.End, event.Zone, event.AllDay, startDate, endDate, c.Owner)
			} else if e == nil && old != event.ETag {
				var same bool
				if e = tx.QueryRow(ctx, "SELECT calendar_id=$3 AND matter_id IS NOT DISTINCT FROM $4::uuid FROM calendar_entries WHERE tenant_id=$1 AND id=$2", tenant, id, calendar, matter).Scan(&same); e != nil {
					return e
				}
				if !same {
					return status.Error(codes.FailedPrecondition, "PROVIDER_ENTRY_OWNERSHIP_CHANGED")
				}
				_, e = tx.Exec(ctx, "UPDATE calendar_entries SET title=$3,description=$4,start_at=$5,end_at=$6,all_day=$7,start_date=$8,end_date=$9,time_zone=$10,status='active',revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, id, event.Title, event.Description, event.Start, event.End, event.AllDay, startDate, endDate, event.Zone)
			}
			if e != nil {
				return e
			}
			if old != event.ETag {
				if e = changed(ctx, tx, tenant, id, "calendar_entries", "calendar.entry.updated.v1"); e != nil {
					return e
				}
			}
			if e = remember(ctx, tx, tenant, connectionID, "calendar_entry", event.ID, id, event.ETag, event, jobID); e != nil {
				return e
			}
		}
		cursor.Pages++
		cursor.Next = page.Next
		if page.Next == "" {
			switch cursor.Kind {
			case "matter":
				cursor.Kind = "calendar"
			case "calendar":
				cursor.Kind = "calendar_entry"
			default:
				cursor.Kind = "reconcile"
			}
		}
		encoded, _ := json.Marshal(cursor)
		if _, err = tx.Exec(ctx, "UPDATE integration_jobs SET cursor=$3,state=$4,updated_at=now(),error_code=NULL WHERE tenant_id=$1 AND id=$2", tenant, jobID, encoded, state); err != nil {
			return err
		}
		if state == "completed" {
			if err = audit(ctx, tx, tenant, who, connectionID, "provider.sync.completed", "snapshot-imported"); err != nil {
				return err
			}
		}
		return nil
	})
	return &pb.JobResponse{Id: jobID, State: state}, err
}
func (s *Service) RunJob(ctx context.Context, r *pb.Ref) (*pb.JobResponse, error) {
	if !validID(r.Id) || !validID(r.TenantId) {
		return nil, bad("INVALID_JOB_REFERENCE")
	}
	var who, kind, resource, state, code string
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT principal_id,kind,resource_id::text,state,coalesce(error_code,'') FROM integration_jobs WHERE tenant_id=$1 AND id=$2", r.TenantId, r.Id).Scan(&who, &kind, &resource, &state, &code)
	})
	if err != nil {
		return nil, err
	}
	if state == "completed" || state == "failed" || state == "rejected" || state == "uncertain" {
		return &pb.JobResponse{Id: r.Id, State: state, ErrorCode: code}, nil
	}
	var reply *pb.JobResponse
	switch kind {
	case "file":
		reply, err = s.processFile(ctx, r.TenantId, resource)
	case "provider_sync":
		reply, err = s.processSync(ctx, r.TenantId, r.Id, resource, who)
	case "provider_calendar":
		reply, err = s.processCalendarWrite(ctx, r.TenantId, r.Id, resource, who)
	case "provider_update":
		reply, err = s.processUpdate(ctx, r.TenantId, r.Id, resource, who)
	default:
		return nil, bad("UNSUPPORTED_JOB_KIND")
	}
	if err != nil && (status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.ResourceExhausted) {
		_ = s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
			if kind == "file" {
				if _, e := tx.Exec(ctx, "UPDATE file_ingestions SET state='rejected',error_code='JOB_AUTHORIZATION_OR_CONTRACT_FAILED' WHERE tenant_id=$1 AND id=$2 AND state IN('queued','processing')", r.TenantId, resource); e != nil {
					return e
				}
			}
			_, e := tx.Exec(ctx, "UPDATE integration_jobs SET state='failed',error_code='JOB_AUTHORIZATION_OR_CONTRACT_FAILED',updated_at=now() WHERE tenant_id=$1 AND id=$2", r.TenantId, r.Id)
			return e
		})
		return &pb.JobResponse{Id: r.Id, State: "failed", ErrorCode: "JOB_AUTHORIZATION_OR_CONTRACT_FAILED"}, nil
	}
	if err == nil && reply != nil && (reply.State == "completed" || reply.State == "rejected") {
		err = s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, "UPDATE integration_jobs SET state=$3,updated_at=now() WHERE tenant_id=$1 AND id=$2", r.TenantId, r.Id, reply.State)
			return e
		})
	}
	return reply, err
}
func (s *Service) FailJob(ctx context.Context, r *pb.Ref) (*pb.JobResponse, error) {
	if !validID(r.Id) {
		return nil, bad("INVALID_JOB")
	}
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		var kind, resource, state string
		if err := tx.QueryRow(ctx, "SELECT kind,resource_id::text,state FROM integration_jobs WHERE tenant_id=$1 AND id=$2 FOR UPDATE", r.TenantId, r.Id).Scan(&kind, &resource, &state); err != nil {
			return err
		}
		if state != "queued" && state != "running" {
			return nil
		}
		next := "failed"
		if kind == "file" {
			if _, err := tx.Exec(ctx, "UPDATE file_ingestions SET state='rejected',error_code='RETRIES_EXHAUSTED' WHERE tenant_id=$1 AND id=$2 AND state IN('queued','processing')", r.TenantId, resource); err != nil {
				return err
			}
		}
		if kind == "provider_calendar" {
			next = "uncertain"
			if _, err := tx.Exec(ctx, "UPDATE provider_calendar_operations SET state='uncertain' WHERE tenant_id=$1 AND id=$2 AND state='inflight'", r.TenantId, resource); err != nil {
				return err
			}
		}
		if kind == "provider_update" {
			next = "uncertain"
			if _, err := tx.Exec(ctx, "UPDATE provider_update_proposals SET state='uncertain' WHERE tenant_id=$1 AND id=$2 AND state='inflight'", r.TenantId, resource); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "UPDATE integration_jobs SET state=$3,error_code='RETRIES_EXHAUSTED',updated_at=now() WHERE tenant_id=$1 AND id=$2", r.TenantId, r.Id, next)
		return err
	})
	return &pb.JobResponse{Id: r.Id, State: "failed", ErrorCode: "RETRIES_EXHAUSTED"}, err
}

func changed(ctx context.Context, tx pgx.Tx, tenant, id, table, kind string) error {
	var revision int64
	if err := tx.QueryRow(ctx, "SELECT revision FROM "+table+" WHERE tenant_id=$1 AND id=$2", tenant, id).Scan(&revision); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1::uuid,$2::text,'1.0',$3::uuid,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','integration-service','type',$2::text,'time',now(),'tenantId',$1::uuid::text,'schemaVersion','1.0','aggregateId',$3::uuid::text,'data',jsonb_build_object('revision',$4::bigint,'decisionCode','provider_sync')))`, tenant, kind, id, revision)
	return err
}
