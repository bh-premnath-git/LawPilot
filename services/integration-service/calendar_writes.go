package integrations

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"io"
	"net/url"
	"strings"
	"time"
)

type appointment struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Start       string `json:"start_at"`
	End         string `json:"end_at"`
	Zone        string `json:"time_zone"`
	Calendar    string `json:"calendar_id"`
	Matter      string `json:"matter_id"`
	OperationID string `json:"operation_id"`
}

func calendarAccess(ctx context.Context, tx pgx.Tx, tenant, id, who, permission string) error {
	var ok bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM calendar_grants WHERE tenant_id=$1 AND calendar_id=$2 AND principal_id=$3 AND permission=$4)", tenant, id, who, permission).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return denied()
	}
	return access(ctx, tx, tenant, who, "calendar."+permission, "")
}
func (s *Service) ProposeCalendarWrite(ctx context.Context, r *pb.CalendarWriteRequest) (*pb.CalendarOperationResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	enabled, _ := requiredFeature("PROVIDER_CALENDAR_WRITES_ENABLED")
	if !s.Writes || !enabled {
		return nil, disabled()
	}
	start, e1 := time.Parse(time.RFC3339, r.StartAt)
	end, e2 := time.Parse(time.RFC3339, r.EndAt)
	_, e3 := time.LoadLocation(r.TimeZone)
	if !validID(r.CalendarId) || !validID(r.MatterId) || (r.Operation != "create" && r.Operation != "update") || e1 != nil || e2 != nil || e3 != nil || r.TimeZone == "" || r.TimeZone == "Local" || !end.After(start) || end.Sub(start) > 24*time.Hour || len(strings.TrimSpace(r.Title)) == 0 || len(r.Title) > 256 || len(r.Description) > 4096 {
		return nil, bad("INVALID_PROVIDER_APPOINTMENT")
	}
	reply := new(pb.CalendarOperationResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		c, e := connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "manage")
		if e != nil {
			return e
		}
		if c.State != "active" {
			return disabled()
		}
		if e = access(ctx, tx, tenant, actor(ctx), "provider.write", r.MatterId); e != nil {
			return e
		}
		if e = calendarAccess(ctx, tx, tenant, r.CalendarId, actor(ctx), "write"); e != nil {
			return e
		}
		if e = calendarAccess(ctx, tx, tenant, r.CalendarId, actor(ctx), "read"); e != nil {
			return e
		}
		if e = lock(ctx, tx, tenant, r.CalendarId); e != nil {
			return e
		}
		hit, e := cached(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, fingerprint("calendar-write", r), reply)
		if e != nil || hit {
			return e
		}
		var calendar, matter string
		var revision int64
		etag := ""
		var entry any
		if e = tx.QueryRow(ctx, "SELECT b.external_id,c.revision FROM provider_sync_bindings b JOIN calendars c ON c.tenant_id=b.tenant_id AND c.id=b.local_id WHERE b.tenant_id=$1 AND b.connection_id=$2 AND b.resource_type='calendar' AND b.local_id=$3 AND NOT b.deleted AND c.authority='provider:'||$4", tenant, r.ConnectionId, r.CalendarId, c.Provider).Scan(&calendar, &revision); e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, "SELECT external_id FROM provider_sync_bindings WHERE tenant_id=$1 AND connection_id=$2 AND resource_type='matter' AND local_id=$3 AND NOT deleted", tenant, r.ConnectionId, r.MatterId).Scan(&matter); e != nil {
			return e
		}
		if r.Operation == "update" {
			if !validID(r.EntryId) {
				return bad("INVALID_ENTRY")
			}
			entry = r.EntryId
			if e = tx.QueryRow(ctx, "SELECT e.revision,b.external_etag FROM calendar_entries e JOIN provider_sync_bindings b ON b.tenant_id=e.tenant_id AND b.local_id=e.id AND b.resource_type='calendar_entry' WHERE e.tenant_id=$1 AND e.id=$2 AND e.calendar_id=$3 AND e.matter_id=$4 AND e.kind IN(1,2) AND NOT e.all_day AND b.connection_id=$5 AND NOT b.deleted AND b.remote_recurrence='' AND NOT b.remote_court_rule", tenant, r.EntryId, r.CalendarId, r.MatterId, r.ConnectionId).Scan(&revision, &etag); e != nil {
				return e
			}
		}
		id := uuid.NewString()
		value := appointment{r.Title, r.Description, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), r.TimeZone, calendar, matter, id}
		raw, _ := json.Marshal(value)
		artifactRaw, _ := json.Marshal([]any{tenant, r.ConnectionId, r.CalendarId, r.MatterId, r.EntryId, r.Operation, revision, etag, value, actor(ctx)})
		hash := sha(artifactRaw)
		if _, e = tx.Exec(ctx, "INSERT INTO provider_calendar_operations(id,tenant_id,connection_id,calendar_id,matter_id,entry_id,operation,payload,artifact_sha256,expected_revision,expected_etag,proposed_by)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", id, tenant, r.ConnectionId, r.CalendarId, r.MatterId, entry, r.Operation, raw, hash, revision, etag, actor(ctx)); e != nil {
			return e
		}
		reply.OperationId = id
		reply.State = "pending"
		reply.ArtifactSha256 = hash // Preview contains immutable scope and preconditions.
		reply.Artifact, _ = structpb.NewStruct(map[string]any{"operation": r.Operation, "calendarId": r.CalendarId, "matterId": r.MatterId, "entryId": r.EntryId, "title": r.Title, "description": r.Description, "startAt": value.Start, "endAt": value.End, "timeZone": r.TimeZone, "expectedRevision": revision, "expectedEtag": etag})
		if e = audit(ctx, tx, tenant, actor(ctx), id, "provider.appointment.proposed", "distinct_review_required"); e != nil {
			return e
		}
		return save(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, fingerprint("calendar-write", r), reply)
	})
	return reply, err
}
func (s *Service) GetCalendarOperation(ctx context.Context, r *pb.CalendarOperationRequest) (*pb.CalendarOperationResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := &pb.CalendarOperationResponse{OperationId: r.OperationId}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var conn, cal, matter, who, operation, entry, etag string
		var revision int64
		var raw []byte
		if e := tx.QueryRow(ctx, "SELECT connection_id::text,calendar_id::text,matter_id::text,proposed_by,operation,coalesce(entry_id::text,''),expected_revision,expected_etag,payload,artifact_sha256,state FROM provider_calendar_operations WHERE tenant_id=$1 AND id=$2", tenant, r.OperationId).Scan(&conn, &cal, &matter, &who, &operation, &entry, &revision, &etag, &raw, &reply.ArtifactSha256, &reply.State); e != nil {
			return e
		}
		permission := "approve"
		if who == actor(ctx) {
			permission = "manage"
		}
		if _, e := connectionAccess(ctx, tx, tenant, conn, actor(ctx), permission); e != nil {
			return e
		}
		if e := access(ctx, tx, tenant, actor(ctx), "provider."+permission, matter); e != nil {
			return e
		}
		if e := calendarAccess(ctx, tx, tenant, cal, actor(ctx), "read"); e != nil {
			return e
		}
		var payload map[string]any
		json.Unmarshal(raw, &payload)
		payload["connectionId"] = conn
		payload["operation"] = operation
		payload["calendarId"] = cal
		payload["matterId"] = matter
		payload["entryId"] = entry
		payload["expectedRevision"] = revision
		payload["expectedEtag"] = etag
		reply.Artifact, _ = structpb.NewStruct(payload)
		return nil
	})
	return reply, err
}
func (s *Service) DecideCalendarWrite(ctx context.Context, r *pb.CalendarOperationRequest) (*pb.CalendarOperationResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	enabled, _ := requiredFeature("PROVIDER_CALENDAR_WRITES_ENABLED")
	if !s.Writes || !enabled {
		return nil, disabled()
	}
	if r.Decision != "approve" && r.Decision != "reject" {
		return nil, bad("INVALID_DECISION")
	}
	reply := new(pb.CalendarOperationResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if e := lock(ctx, tx, tenant, r.OperationId); e != nil {
			return e
		}
		var conn, cal, matter, who, hash, state string
		if e := tx.QueryRow(ctx, "SELECT connection_id::text,calendar_id::text,matter_id::text,proposed_by,artifact_sha256,state FROM provider_calendar_operations WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, r.OperationId).Scan(&conn, &cal, &matter, &who, &hash, &state); e != nil {
			return e
		}
		if _, e := connectionAccess(ctx, tx, tenant, conn, actor(ctx), "approve"); e != nil {
			return e
		}
		if e := access(ctx, tx, tenant, actor(ctx), "provider.approve", matter); e != nil {
			return e
		}
		if e := calendarAccess(ctx, tx, tenant, cal, actor(ctx), "approve"); e != nil {
			return e
		}
		if e := calendarAccess(ctx, tx, tenant, cal, actor(ctx), "read"); e != nil {
			return e
		}
		if who == actor(ctx) {
			return denied()
		}
		if hash != r.ArtifactSha256 {
			return status.Error(codes.Aborted, "ARTIFACT_CHANGED")
		}
		hit, e := cached(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, fingerprint("calendar-decision", r), reply)
		if e != nil || hit {
			return e
		}
		if state != "pending" {
			return status.Error(codes.FailedPrecondition, "ALREADY_DECIDED")
		}
		state = "rejected"
		if r.Decision == "approve" {
			state = "approved"
			if e = job(ctx, tx, tenant, who, r.OperationId, "provider_calendar", r.OperationId); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "UPDATE provider_calendar_operations SET state=$3,reviewed_by=$4 WHERE tenant_id=$1 AND id=$2", tenant, r.OperationId, state, actor(ctx)); e != nil {
			return e
		}
		reply.OperationId = r.OperationId
		reply.State = state
		reply.ArtifactSha256 = hash
		if e = audit(ctx, tx, tenant, actor(ctx), r.OperationId, "provider.appointment.decided", state); e != nil {
			return e
		}
		return save(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, fingerprint("calendar-decision", r), reply)
	})
	return reply, err
}
func (p *Provider) writeAppointment(ctx context.Context, a appointment, operation, external, etag, token string) (string, string, error) {
	target := p.Base + p.resourcePath("calendar_entry")
	method := "POST"
	if operation == "update" {
		method = "PATCH"
		target = strings.TrimSuffix(target, ".json") + "/" + url.PathEscape(external)
		if p.ID == "clio-manage" {
			target += ".json"
		}
	}
	data := map[string]any{"title": a.Title, "description": a.Description, "start_at": a.Start, "end_at": a.End, "time_zone": a.Zone, "calendar_id": a.Calendar, "matter_id": a.Matter, "operation_id": a.OperationID, "all_day": false}
	if p.ID == "clio-manage" {
		parts := strings.SplitN(a.Calendar, ":", 2)
		if len(parts) != 2 {
			return "", "", bad("INVALID_CALENDAR_MAPPING")
		}
		data = map[string]any{"summary": a.Title, "description": a.Description, "start_at": a.Start, "end_at": a.End, "all_day": false, "calendar_owner": map[string]any{"id": json.Number(parts[1])}, "matter": map[string]any{"id": json.Number(a.Matter)}, "send_email_notification": false}
		if operation == "create" {
			data["external_properties"] = []any{map[string]any{"name": "lawpilot_operation", "value": a.OperationID}}
		}
	}
	raw, _ := json.Marshal(map[string]any{"data": data})
	response, err := p.request(ctx, method, target, token, etag, raw)
	if err != nil {
		return "uncertain", "", nil
	}
	defer response.Body.Close()
	if response.StatusCode == 412 {
		return "conflict", "", nil
	}
	if response.StatusCode != 200 && response.StatusCode != 201 {
		return "uncertain", "", nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 65537))
	var result struct {
		Data struct {
			ID json.RawMessage `json:"id"`
		} `json:"data"`
	}
	if len(body) > 65536 || json.Unmarshal(body, &result) != nil || scalar(result.Data.ID) == "" {
		return "uncertain", "", nil
	}
	return "acknowledged", scalar(result.Data.ID), nil
}
func (s *Service) processCalendarWrite(ctx context.Context, tenant, jobID, id, who string) (*pb.JobResponse, error) {
	enabled, _ := requiredFeature("PROVIDER_CALENDAR_WRITES_ENABLED")
	if !s.Writes || !enabled {
		return nil, disabled()
	}
	var conn, cal, matter, operation, external, etag, state, reviewer string
	var raw []byte
	var expected int64
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if e := lock(ctx, tx, tenant, id); e != nil {
			return e
		}
		var entry *string
		if e := tx.QueryRow(ctx, "SELECT connection_id::text,calendar_id::text,matter_id::text,entry_id::text,operation,payload,expected_revision,expected_etag,state,coalesce(reviewed_by,''),coalesce(remote_id,'') FROM provider_calendar_operations WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, id).Scan(&conn, &cal, &matter, &entry, &operation, &raw, &expected, &etag, &state, &reviewer, &external); e != nil {
			return e
		}
		if _, e := connectionAccess(ctx, tx, tenant, conn, who, "manage"); e != nil {
			return e
		}
		if _, e := connectionAccess(ctx, tx, tenant, conn, reviewer, "approve"); e != nil {
			return e
		}
		if who == reviewer || reviewer == "" {
			return denied()
		}
		for _, a := range []struct{ Who, Permission, Capability string }{{who, "write", "provider.write"}, {reviewer, "approve", "provider.approve"}} {
			if e := access(ctx, tx, tenant, a.Who, a.Capability, matter); e != nil {
				return e
			}
			if e := calendarAccess(ctx, tx, tenant, cal, a.Who, "read"); e != nil {
				return e
			}
			if e := calendarAccess(ctx, tx, tenant, cal, a.Who, a.Permission); e != nil {
				return e
			}
		}
		if state != "approved" && state != "inflight" {
			return status.Error(codes.FailedPrecondition, "OPERATION_NOT_APPROVED")
		}
		if state == "approved" {
			var revision int64
			if operation == "update" {
				if e := tx.QueryRow(ctx, "SELECT e.revision,b.external_id FROM calendar_entries e JOIN provider_sync_bindings b ON b.tenant_id=e.tenant_id AND b.local_id=e.id AND b.resource_type='calendar_entry' WHERE e.tenant_id=$1 AND e.id=$2 AND e.calendar_id=$3 AND e.matter_id=$4 AND NOT b.deleted AND b.remote_recurrence='' AND NOT b.remote_court_rule", tenant, entry, cal, matter).Scan(&revision, &external); e != nil {
					return e
				}
			} else {
				if e := tx.QueryRow(ctx, "SELECT revision FROM calendars WHERE tenant_id=$1 AND id=$2", tenant, cal).Scan(&revision); e != nil {
					return e
				}
			}
			if expected != revision {
				return status.Error(codes.FailedPrecondition, "CALENDAR_SOURCE_CHANGED")
			}
			_, e := tx.Exec(ctx, "UPDATE provider_calendar_operations SET state='inflight',remote_id=NULLIF($3,'') WHERE tenant_id=$1 AND id=$2", tenant, id, external)
			return e
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var a appointment
	if json.Unmarshal(raw, &a) != nil {
		return nil, bad("INVALID_ARTIFACT")
	}
	p, token, err := s.token(ctx, tenant, conn, who, "manage")
	if err != nil {
		return nil, err
	}
	outcome := "uncertain"
	if state == "approved" {
		outcome, external, err = p.writeAppointment(ctx, a, operation, external, etag, token)
		if err != nil {
			return nil, err
		}
	}
	// A fresh list is the reconciliation authority. Creates carry an external
	// operation marker; lost acknowledgements never trigger a second POST.
	target, _ := p.safeURL("calendar_entry", "")
	if operation == "create" {
		u, _ := url.Parse(target)
		q := u.Query()
		q.Set("external_property_name", "lawpilot_operation")
		q.Set("external_property_value", id)
		u.RawQuery = q.Encode()
		target = u.String()
	}
	page, e := p.Fetch(ctx, "calendar_entry", target, token)
	if e == nil {
		for _, event := range page.Entries {
			matchesID := external != "" && event.ID == external
			if operation == "create" && p.ID == "generic-rest" {
				matchesID = event.OperationID == id
			}
			if (matchesID || operation == "create" && p.ID == "clio-manage" && len(page.Entries) == 1) && event.Title == a.Title && event.Description == a.Description && event.Start.Format(time.RFC3339) == a.Start && event.End.Format(time.RFC3339) == a.End && event.Calendar == a.Calendar && event.Matter == a.Matter {
				external = event.ID
				outcome = "completed"
				break
			}
		}
	}
	if outcome == "acknowledged" {
		outcome = "uncertain"
	}
	jobState := outcome
	if outcome == "conflict" {
		jobState = "failed"
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var current string
		if e := tx.QueryRow(ctx, "SELECT state FROM provider_calendar_operations WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, id).Scan(&current); e != nil {
			return e
		}
		if current == "completed" {
			jobState = "completed"
			return nil
		}
		if _, e := tx.Exec(ctx, "UPDATE provider_calendar_operations SET state=$3,remote_id=NULLIF($4,'') WHERE tenant_id=$1 AND id=$2", tenant, id, outcome, external); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "UPDATE integration_jobs SET state=$3,error_code=CASE WHEN $3='uncertain' THEN 'PROVIDER_WRITE_UNCERTAIN_REQUIRES_REVIEW' ELSE NULL END,updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, jobID, jobState); e != nil {
			return e
		}
		if outcome == "completed" {
			_, e := tx.Exec(ctx, "INSERT INTO provider_sync_signals(tenant_id,connection_id,principal_id)VALUES($1,$2,$3)ON CONFLICT(connection_id)DO UPDATE SET generation=provider_sync_signals.generation+1,principal_id=excluded.principal_id", tenant, conn, who)
			if e != nil {
				return e
			}
		}
		return audit(ctx, tx, tenant, who, id, "provider.appointment.result", outcome)
	})
	return &pb.JobResponse{Id: jobID, State: jobState}, err
}
