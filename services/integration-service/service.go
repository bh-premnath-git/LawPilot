package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	ctxpb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"os"
	"strings"
)

type Service struct {
	pb.UnimplementedIntegrationServiceServer
	Pool      *pgxpool.Pool
	Storage   ObjectStore
	Scanner   Scanner
	Parser    pb.DocumentParserClient
	Context   ctxpb.ContextServiceClient
	Providers map[string]*Provider
	Keys      *Keyring
	Writes    bool
}

func requiredFeature(name string) (bool, error) {
	v := os.Getenv(name)
	if v == "" || v == "false" {
		return false, nil
	}
	if v == "true" {
		return true, nil
	}
	return false, errors.New("invalid feature setting")
}
func sha(text []byte) string { h := sha256.Sum256(text); return hex.EncodeToString(h[:]) }
func validID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && strings.EqualFold(parsed.String(), id)
}
func bad(code string) error         { return status.Error(codes.InvalidArgument, code) }
func denied() error                 { return status.Error(codes.PermissionDenied, "ACCESS_DENIED") }
func unavailable(code string) error { return status.Error(codes.Unavailable, code) }
func disabled() error {
	return status.Error(codes.FailedPrecondition, "INTEGRATION_DISABLED_OR_UNCONFIGURED")
}
func scope(s *pb.Scope) (string, error) {
	if s == nil || !validID(s.TenantId) {
		return "", bad("INVALID_TENANT")
	}
	return strings.ToLower(s.TenantId), nil
}
func (s *Service) tx(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	if !validID(tenant) {
		return bad("INVALID_TENANT")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return unavailable("DATABASE_UNAVAILABLE")
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
		return status.Error(codes.NotFound, "RESOURCE_NOT_FOUND")
	}
	return unavailable("INTEGRATION_TRANSACTION_FAILED")
}
func access(ctx context.Context, tx pgx.Tx, tenant, who, cap, matter string) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships m JOIN principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id WHERE m.tenant_id=$1 AND m.principal_id=$2 AND m.status='active' AND c.capability_id=$3)`, tenant, who, cap).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return denied()
	}
	if matter != "" {
		if !validID(matter) {
			return bad("INVALID_MATTER")
		}
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM matter_grants WHERE tenant_id=$1 AND matter_id=$2 AND principal_id=$3 AND permission='matter.read')", tenant, matter, who).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return denied()
		}
	}
	return nil
}
func lock(ctx context.Context, tx pgx.Tx, tenant, id string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", strings.ToLower(tenant)+":"+strings.ToLower(id))
	return err
}
func fingerprint(method string, msg proto.Message) string {
	clone := proto.Clone(msg).ProtoReflect()
	field := clone.Descriptor().Fields().ByName("scope")
	if field != nil && clone.Has(field) {
		scope := clone.Get(field).Message()
		scope.Clear(scope.Descriptor().Fields().ByName("request_id"))
	}
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(clone.Interface())
	return sha(append([]byte(method+":"), raw...))
}
func cached(ctx context.Context, tx pgx.Tx, tenant, who, key, hash string, result proto.Message) (bool, error) {
	if len(key) < 8 || len(key) > 128 {
		return false, bad("INVALID_IDEMPOTENCY_KEY")
	}
	var stored string
	var raw []byte
	err := tx.QueryRow(ctx, "SELECT request_sha256,response FROM integration_commands WHERE tenant_id=$1 AND principal_id=$2 AND idempotency_key=$3", tenant, who, key).Scan(&stored, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if hash != stored {
		return false, status.Error(codes.AlreadyExists, "IDEMPOTENCY_CONFLICT")
	}
	return true, proto.Unmarshal(raw, result)
}
func save(ctx context.Context, tx pgx.Tx, tenant, who, key, hash string, result proto.Message) error {
	raw, err := proto.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO integration_commands VALUES($1,$2,$3,$4,$5)", tenant, who, key, hash, raw)
	return err
}
func audit(ctx context.Context, tx pgx.Tx, tenant, who, id, action, code string) error {
	_, err := tx.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_id,resource_id,action,decision_code)VALUES($1,$2,$3,$4,$5)", tenant, who, id, action, code)
	return err
}
func job(ctx context.Context, tx pgx.Tx, tenant, who, id, kind, resource string) error {
	workflowID := "lawpilot:" + tenant + ":" + kind + ":" + id
	if _, err := tx.Exec(ctx, "INSERT INTO integration_jobs(id,tenant_id,principal_id,kind,resource_id,workflow_id)VALUES($1,$2,$3,$4,$5,$6)", id, tenant, who, kind, resource, workflowID); err != nil {
		return err
	}
	event := map[string]string{"file": "file.process.requested.v1", "provider_sync": "provider.sync.requested.v1", "provider_update": "provider.update.requested.v1", "provider_calendar": "provider.update.requested.v1", "legal_review": "legal.review.requested.v1"}[kind]
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1,$2,'1.0',$3,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','integration-service','type',$2::text,'time',now(),'tenantId',$1::uuid::text,'schemaVersion','1.0','aggregateId',$3::uuid::text,'data',jsonb_build_object('state','queued')))`, tenant, event, id)
	return err
}
func (s *Service) Status(ctx context.Context, r *pb.StatusRequest) (*pb.StatusResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	result := &pb.StatusResponse{StorageEnabled: s.Storage != nil, ClioEnabled: s.Providers["clio-manage"] != nil, GenericProviderEnabled: s.Providers["generic-rest"] != nil}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error { return access(ctx, tx, tenant, actor(ctx), "integration.manage", "") })
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"clio-manage", "generic-rest"} {
		if s.Providers[name] != nil {
			result.Providers = append(result.Providers, name)
		}
	}
	return result, nil
}
func (s *Service) GetJob(ctx context.Context, r *pb.JobRequest) (*pb.JobResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	result := &pb.JobResponse{Id: r.JobId}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, actor(ctx), "integration.read", ""); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT state,coalesce(error_code,'') FROM integration_jobs WHERE tenant_id=$1 AND id=$2 AND principal_id=$3", tenant, r.JobId, actor(ctx)).Scan(&result.State, &result.ErrorCode)
	})
	return result, err
}
