package contextengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"strings"
	"time"
)

type Service struct {
	pb.UnimplementedContextServiceServer
	Pool     *pgxpool.Pool
	Embedder *Embedder
	Now      func() time.Time
}

func New(pool *pgxpool.Pool) (*Service, error) {
	embedder, err := configuredEmbedder()
	return &Service{Pool: pool, Embedder: embedder}, err
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func digest(text string) string { h := sha256.Sum256([]byte(text)); return hex.EncodeToString(h[:]) }
func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && strings.EqualFold(id.String(), value)
}
func bad(message string) error { return status.Error(codes.InvalidArgument, message) }
func denied() error            { return status.Error(codes.PermissionDenied, "ACCESS_DENIED") }
func notFound() error          { return status.Error(codes.NotFound, "SOURCE_NOT_FOUND") }
func scope(value *pb.Scope) (string, error) {
	if value == nil || !validID(value.TenantId) {
		return "", bad("INVALID_TENANT")
	}
	return strings.ToLower(value.TenantId), nil
}
func (s *Service) tx(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	if !validID(tenant) {
		return bad("INVALID_TENANT")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return status.Error(codes.Unavailable, "DATABASE_UNAVAILABLE")
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", strings.ToLower(tenant)); err == nil {
		_, err = tx.Exec(ctx, "SET LOCAL statement_timeout='10s'")
	}
	if err == nil {
		_, err = tx.Exec(ctx, "SET LOCAL lock_timeout='3s'")
	}
	if err == nil {
		err = fn(tx)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound()
	}
	return status.Error(codes.Unavailable, "CONTEXT_TRANSACTION_FAILED")
}
func access(ctx context.Context, tx pgx.Tx, tenant, matter, who, permission string) error {
	if !validID(matter) || strings.TrimSpace(who) == "" {
		return denied()
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships m JOIN principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id JOIN matter_grants g ON g.tenant_id=m.tenant_id AND g.principal_id=m.principal_id WHERE m.tenant_id=$1 AND m.principal_id=$2 AND m.status='active' AND c.capability_id=$3 AND g.matter_id=$4 AND g.permission='matter.read')`, tenant, who, permission, matter).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return denied()
	}
	if permission == "context.read" {
		var library, knowledge bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knowledge_libraries WHERE tenant_id=$1 AND matter_id=$2),EXISTS(SELECT 1 FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$3 AND capability_id='knowledge.read')", tenant, matter, who).Scan(&library, &knowledge); err != nil {
			return err
		}
		if library && !knowledge {
			return denied()
		}
	}
	return nil
}
func lockMatter(ctx context.Context, tx pgx.Tx, tenant, matter string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", strings.ToLower(tenant)+":"+strings.ToLower(matter))
	return err
}
func commandHash(method string, message proto.Message) string {
	copy := proto.Clone(message).ProtoReflect()
	field := copy.Descriptor().Fields().ByName("scope")
	if field != nil && copy.Has(field) {
		request := copy.Get(field).Message()
		request.Clear(request.Descriptor().Fields().ByName("request_id"))
	}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(copy.Interface())
	return digest(method + string(raw))
}
func cached(ctx context.Context, tx pgx.Tx, tenant, who, key, hash string, response proto.Message) (bool, error) {
	if len(key) < 8 || len(key) > 128 {
		return false, bad("INVALID_IDEMPOTENCY_KEY")
	}
	var stored string
	var body []byte
	err := tx.QueryRow(ctx, "SELECT request_sha256,response FROM context_commands WHERE tenant_id=$1 AND principal_id=$2 AND idempotency_key=$3", tenant, who, key).Scan(&stored, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored != hash {
		return false, status.Error(codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
	}
	return true, proto.Unmarshal(body, response)
}
func save(ctx context.Context, tx pgx.Tx, tenant, who, key, hash string, response proto.Message) error {
	body, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO context_commands VALUES($1,$2,$3,$4,$5)", tenant, who, key, hash, body)
	return err
}
func emit(ctx context.Context, tx pgx.Tx, tenant, who, id, kind string, revision int64) error {
	if _, err := tx.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code)VALUES($1,$2,$3,$4,'native-text')", tenant, who, kind, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1,$2,'1.0',$3,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','context-engine','type',$2::text,'time',now(),'tenantId',$1::uuid::text,'schemaVersion','1.0','aggregateId',$3::uuid::text,'data',jsonb_build_object('revision',$4::bigint,'decisionCode','native-text')))`, tenant, kind, id, revision)
	return err
}
