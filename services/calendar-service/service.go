package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Service struct {
	pb.UnimplementedCalendarServiceServer
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func identifier(id string) bool { _, err := uuid.Parse(id); return err == nil }
func rpcError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var db *pgconn.PgError
	if errors.As(err, &db) {
		switch db.Code {
		case "23505", "23P01":
			return status.Error(codes.AlreadyExists, "CONFLICT")
		case "40001", "40P01", "55P03", "57014":
			return status.Error(codes.Aborted, "RETRYABLE_CONFLICT")
		}
	}
	return status.Error(codes.Unavailable, "DATABASE_UNAVAILABLE")
}
func (s *Service) tx(ctx context.Context, tenant, calendar string, write bool, fn func(pgx.Tx) error) error {
	if !identifier(tenant) || calendar != "" && !identifier(calendar) {
		return bad("INVALID_RESOURCE_ID")
	}
	tenant = strings.ToLower(tenant)
	calendar = strings.ToLower(calendar)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return rpcError(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", strings.ToLower(tenant)); err != nil {
		return rpcError(err)
	}
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='10s'"); err != nil {
		return rpcError(err)
	}
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='3s'"); err != nil {
		return rpcError(err)
	}
	if write {
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", tenant+":"+calendar); err != nil {
			return rpcError(err)
		}
	}
	if err = fn(tx); err != nil {
		return rpcError(err)
	}
	return rpcError(tx.Commit(ctx))
}
func member(ctx context.Context, tx pgx.Tx, tenant, who string) error {
	if strings.TrimSpace(who) == "" {
		return status.Error(codes.Unauthenticated, "ACCESS_TOKEN_REQUIRED")
	}
	var ok bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM memberships WHERE tenant_id=$1 AND principal_id=$2 AND status='active')", tenant, who).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
	}
	return nil
}
func capability(ctx context.Context, tx pgx.Tx, tenant, who, cap string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id=$3)", tenant, who, cap).Scan(&ok)
	return ok, err
}
func calendarRecord(ctx context.Context, tx pgx.Tx, tenant, id string) (*pb.Calendar, error) {
	value := new(pb.Calendar)
	err := tx.QueryRow(ctx, "SELECT id::text,name,default_time_zone,approval_required,revision,authority FROM calendars WHERE tenant_id=$1 AND id=$2", tenant, id).Scan(&value.Id, &value.Name, &value.DefaultTimeZone, &value.ApprovalRequired, &value.Revision, &value.Authority)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "NOT_FOUND")
	}
	return value, err
}
func authorize(ctx context.Context, tx pgx.Tx, tenant, id, who, action string) (*pb.Calendar, bool, error) {
	if err := member(ctx, tx, tenant, who); err != nil {
		return nil, false, err
	}
	staff, err := capability(ctx, tx, tenant, who, "calendar.read")
	if err != nil {
		return nil, false, err
	}
	client := false
	if !staff && action == "read" {
		client, err = capability(ctx, tx, tenant, who, "calendar.client_read")
		if err != nil {
			return nil, false, err
		}
	}
	if !staff && !client {
		return nil, false, status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
	}
	if action != "read" {
		ok, err := capability(ctx, tx, tenant, who, "calendar."+action)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
		}
	}
	var allowed bool
	err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM calendar_grants WHERE tenant_id=$1 AND calendar_id=$2 AND principal_id=$3 AND permission=$4)", tenant, id, who, action).Scan(&allowed)
	if err != nil {
		return nil, false, err
	}
	if !allowed {
		return nil, false, status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
	}
	if action != "read" {
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM calendar_grants WHERE tenant_id=$1 AND calendar_id=$2 AND principal_id=$3 AND permission='read')", tenant, id, who).Scan(&allowed); err != nil {
			return nil, false, err
		}
		if !allowed {
			return nil, false, status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
		}
	}
	value, err := calendarRecord(ctx, tx, tenant, id)
	return value, client, err
}
func matterAccess(ctx context.Context, tx pgx.Tx, tenant, who, matter string) error {
	if matter == "" {
		return nil
	}
	if !identifier(matter) {
		return bad("INVALID_MATTER_ID")
	}
	var ok bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2 AND matter_id=$3 AND permission='matter.read')", tenant, who, matter).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return status.Error(codes.PermissionDenied, "MATTER_ACCESS_DENIED")
	}
	return nil
}
func scope(value *pb.RequestScope) (string, error) {
	if value == nil || !identifier(value.TenantId) || value.RequestId == "" || len(value.RequestId) > 200 {
		return "", bad("INVALID_CONTEXT")
	}
	return strings.ToLower(value.TenantId), nil
}
func fingerprint(method string, message proto.Message) string {
	clone := proto.Clone(message).ProtoReflect()
	field := clone.Descriptor().Fields().ByName("scope")
	if field != nil && clone.Has(field) {
		request := clone.Get(field).Message()
		request.Clear(request.Descriptor().Fields().ByName("request_id"))
	}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(clone.Interface())
	sum := sha256.Sum256(append([]byte(method+":"), raw...))
	return hex.EncodeToString(sum[:])
}
func cached(ctx context.Context, tx pgx.Tx, tenant, who, key, hash string, response proto.Message) (bool, error) {
	if len(key) < 8 || len(key) > 128 {
		return false, bad("INVALID_IDEMPOTENCY_KEY")
	}
	var original string
	var body []byte
	err := tx.QueryRow(ctx, "SELECT request_sha256,response FROM calendar_commands WHERE tenant_id=$1 AND principal_id=$2 AND idempotency_key=$3", tenant, who, key).Scan(&original, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if original != hash {
		return false, status.Error(codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
	}
	return true, proto.Unmarshal(body, response)
}
func save(ctx context.Context, tx pgx.Tx, tenant, who, key, hash string, response proto.Message) error {
	body, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO calendar_commands(tenant_id,principal_id,idempotency_key,request_sha256,response)VALUES($1,$2,$3,$4,$5)", tenant, who, key, hash, body)
	return err
}
func event(ctx context.Context, tx pgx.Tx, tenant, who, id, kind string, revision int64, code string) error {
	_, err := tx.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code)VALUES($1,$2,$3,$4,$5)", tenant, who, kind, id, code)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1::uuid,$2::text,'1.0',$3::uuid,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','calendar-service','type',$2::text,'time',now(),'tenantId',$1::uuid::text,'schemaVersion','1.0','aggregateId',$3::uuid::text,'data',jsonb_build_object('revision',$4::bigint,'decisionCode',$5::text)))`, tenant, kind, id, revision, code)
	return err
}
