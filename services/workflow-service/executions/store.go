package executions

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Ref struct {
	TenantID    string `json:"tenantId"`
	ExecutionID string `json:"executionId"`
}
type Execution struct {
	Ref
	PrincipalID  string
	CapabilityID string
	MatterID     *string
	Status       string
	Input        json.RawMessage
}
type Store struct{ Pool *pgxpool.Pool }

func (s *Store) transaction(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true)", tenant); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='10s'"); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func terminal(status string) bool {
	return status == "completed" || status == "rejected" || status == "cancelled" || status == "failed" || status == "awaiting_review"
}
func (s *Store) load(ctx context.Context, tx pgx.Tx, ref Ref) (Execution, error) {
	e := Execution{Ref: ref}
	err := tx.QueryRow(ctx, `SELECT principal_id,capability_id,matter_id::text,status,request_input FROM ai_executions WHERE tenant_id=$1 AND id=$2 AND workflow_id IS NOT NULL FOR UPDATE`, ref.TenantID, ref.ExecutionID).Scan(&e.PrincipalID, &e.CapabilityID, &e.MatterID, &e.Status, &e.Input)
	return e, err
}

var ErrPermission = errors.New("PERMISSION_DENIED")

func permitted(ctx context.Context, tx pgx.Tx, e Execution) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships WHERE tenant_id=$1 AND principal_id=$2 AND status='active')
 AND EXISTS(SELECT 1 FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id=$3)
 AND ($4::uuid IS NULL OR EXISTS(SELECT 1 FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2 AND matter_id=$4 AND permission='matter.read'))`, e.TenantID, e.PrincipalID, e.CapabilityID, e.MatterID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrPermission
	}
	return nil
}
func appendEvent(ctx context.Context, tx pgx.Tx, e Execution, state, code string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code) VALUES($1,$2,$3,$4,$5)`, e.TenantID, e.PrincipalID, "ai."+state, e.ExecutionID, code)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)
 VALUES($1,$2,'1.0',$3,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','workflow-service','type',$2::text,'time',now(),'tenantId',$1::uuid::text,'schemaVersion','1.0','aggregateId',$3::uuid::text,'data',jsonb_build_object('executionId',$3::uuid::text,'state',$4::text,'decisionCode',$5::text)))`, e.TenantID, "lawpilot.ai."+state+".v1", e.ExecutionID, state, code)
	return err
}
func (s *Store) Begin(ctx context.Context, ref Ref) (Execution, error) {
	var e Execution
	err := s.transaction(ctx, ref.TenantID, func(tx pgx.Tx) error {
		var err error
		e, err = s.load(ctx, tx, ref)
		if err != nil {
			return err
		}
		if terminal(e.Status) {
			return nil
		}
		if err = permitted(ctx, tx, e); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE ai_executions SET status='running',activity_attempts=activity_attempts+1,updated_at=now() WHERE tenant_id=$1 AND id=$2", ref.TenantID, ref.ExecutionID)
		if err != nil {
			return err
		}
		if e.Status != "running" {
			if err = appendEvent(ctx, tx, e, "running", "running"); err != nil {
				return err
			}
		}
		e.Status = "running"
		return nil
	})
	return e, err
}
func (s *Store) Finish(ctx context.Context, ref Ref, status, code string, result json.RawMessage, reauthorize bool) (string, error) {
	state := ""
	err := s.transaction(ctx, ref.TenantID, func(tx pgx.Tx) error {
		e, err := s.load(ctx, tx, ref)
		if err != nil {
			return err
		}
		state = e.Status
		if terminal(state) {
			return nil
		}
		if reauthorize {
			if err = permitted(ctx, tx, e); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE ai_executions SET status=$3,error_code=CASE WHEN $3='failed' THEN NULLIF($4,'') ELSE NULL END,result=$5,updated_at=now(),completed_at=CASE WHEN $3='awaiting_review' THEN NULL ELSE now() END WHERE tenant_id=$1 AND id=$2`, ref.TenantID, ref.ExecutionID, status, code, []byte(result))
		if err != nil {
			return err
		}
		if err = appendEvent(ctx, tx, e, status, code); err != nil {
			return err
		}
		state = status
		return nil
	})
	return state, err
}
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.MaxConns = 10
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, "SET statement_timeout='15s'")
		return err
	}
	return pgxpool.NewWithConfig(ctx, config)
}
