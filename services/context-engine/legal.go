package contextengine

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"time"
)

func tenantCapability(ctx context.Context, tx pgx.Tx, tenant, who, cap string) error {
	var ok bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM memberships m JOIN principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id WHERE m.tenant_id=$1 AND m.principal_id=$2 AND m.status='active' AND c.capability_id=$3)", tenant, who, cap).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return denied()
	}
	return nil
}
func (s *Service) CreateTemplate(ctx context.Context, r *pb.TemplateRequest) (*pb.TemplateResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(r.Name)) < 1 || len(r.Name) > 120 || r.Version < 1 || len(r.Body) < 1 || len(r.Body) > 16384 {
		return nil, bad("INVALID_TEMPLATE")
	}
	reply := new(pb.TemplateResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := tenantCapability(ctx, tx, tenant, actor(ctx), "template.manage"); err != nil {
			return err
		}
		if err := lockMatter(ctx, tx, tenant, tenant); err != nil {
			return err
		}
		hash := commandHash("create-template", r)
		hit, err := cached(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		id := uuid.NewString()
		if _, err = tx.Exec(ctx, "INSERT INTO legal_template_versions(id,tenant_id,name,version,body,body_sha256,created_by)VALUES($1,$2,$3,$4,$5,$6,$7)", id, tenant, r.Name, r.Version, r.Body, digest(r.Body), actor(ctx)); err != nil {
			return err
		}
		reply.Template = &pb.ApprovedTemplate{Id: id, Sha256: digest(r.Body), Body: r.Body}
		reply.State = "pending"
		return save(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (s *Service) DecideTemplate(ctx context.Context, r *pb.TemplateDecisionRequest) (*pb.TemplateResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if !validID(r.TemplateId) || (r.Decision != "approve" && r.Decision != "revoke") {
		return nil, bad("INVALID_TEMPLATE_DECISION")
	}
	reply := new(pb.TemplateResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := tenantCapability(ctx, tx, tenant, actor(ctx), "template.approve"); err != nil {
			return err
		}
		var body, hash, creator, state string
		if err := tx.QueryRow(ctx, "SELECT body,body_sha256,created_by,state FROM legal_template_versions WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, r.TemplateId).Scan(&body, &hash, &creator, &state); err != nil {
			return err
		}
		if hash != r.ExpectedSha256 || digest(body) != hash {
			return status.Error(codes.Aborted, "TEMPLATE_CHANGED")
		}
		if actor(ctx) == creator && r.Decision == "approve" {
			return status.Error(codes.PermissionDenied, "DISTINCT_TEMPLATE_APPROVER_REQUIRED")
		}
		state = "revoked"
		if r.Decision == "approve" {
			state = "approved"
		}
		if _, err := tx.Exec(ctx, "UPDATE legal_template_versions SET state=$3,approved_by=$4 WHERE tenant_id=$1 AND id=$2", tenant, r.TemplateId, state, actor(ctx)); err != nil {
			return err
		}
		reply.Template = &pb.ApprovedTemplate{Id: r.TemplateId, Sha256: hash, Body: body}
		reply.State = state
		_, err := tx.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code)VALUES($1,$2,'legal.template.decided',$3,$4)", tenant, actor(ctx), r.TemplateId, state)
		return err
	})
	return reply, err
}
func (s *Service) PrepareExecutionContext(ctx context.Context, r *pb.ExecutionRef) (*pb.PreparedContext, error) {
	if !validID(r.ExecutionId) || !validID(r.TenantId) {
		return nil, bad("INVALID_EXECUTION_REFERENCE")
	}
	var who, matter, cap, inputHash string
	var raw []byte
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT principal_id,matter_id::text,capability_id,request_input,encode(digest(convert_to(request_input::jsonb::text,'UTF8'),'sha256'),'hex') FROM ai_executions WHERE tenant_id=$1 AND id=$2 AND status='running'", r.TenantId, r.ExecutionId).Scan(&who, &matter, &cap, &raw, &inputHash); err != nil {
			return err
		}
		if err := access(ctx, tx, r.TenantId, matter, who, cap); err != nil {
			return err
		}
		return access(ctx, tx, r.TenantId, matter, who, "context.read")
	})
	if err != nil {
		return nil, err
	}
	var input struct {
		Question     string `json:"question"`
		DocumentID   string `json:"documentId"`
		TemplateID   string `json:"templateId"`
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return nil, bad("INVALID_LEGAL_INPUT")
	}
	recipe, query, document := "matter.qa.v1", input.Question, input.DocumentID
	var template *pb.ApprovedTemplate
	switch cap {
	case "document.summarize.v1":
		recipe = "document.summary.v1"
	case "matter.qa.v1":
	case "legal.draft.v1":
		query = input.Instructions
		if !validID(input.TemplateID) {
			return nil, bad("APPROVED_TEMPLATE_REQUIRED")
		}
		template = new(pb.ApprovedTemplate)
		err = s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
			if err := access(ctx, tx, r.TenantId, matter, who, cap); err != nil {
				return err
			}
			var approver string
			if err := tx.QueryRow(ctx, "SELECT id::text,body_sha256,body,approved_by FROM legal_template_versions WHERE tenant_id=$1 AND id=$2 AND state='approved'", r.TenantId, input.TemplateID).Scan(&template.Id, &template.Sha256, &template.Body, &approver); err != nil {
				return err
			}
			if digest(template.Body) != template.Sha256 {
				return status.Error(codes.DataLoss, "TEMPLATE_INTEGRITY_FAILED")
			}
			return tenantCapability(ctx, tx, r.TenantId, approver, "template.approve")
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, status.Error(codes.Unimplemented, "LEGAL_RECIPE_UNSUPPORTED")
	}
	trusted := context.WithValue(ctx, principalKey{}, who)
	built, err := s.BuildContext(trusted, &pb.BuildContextRequest{Scope: &pb.Scope{TenantId: r.TenantId, RequestId: r.ExecutionId}, MatterId: matter, RecipeId: recipe, Query: query, DocumentId: document})
	if err != nil {
		return nil, err
	}
	if len(built.Package.Sources) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "EVIDENCE_REQUIRED")
	}
	grantID := uuid.NewString()
	err = s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		if err := access(ctx, tx, r.TenantId, matter, who, cap); err != nil {
			return err
		}
		var stillRunning bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM ai_executions WHERE tenant_id=$1 AND id=$2 AND status='running')", r.TenantId, r.ExecutionId).Scan(&stillRunning); err != nil {
			return err
		}
		if !stillRunning {
			return status.Error(codes.FailedPrecondition, "EXECUTION_NOT_RUNNING")
		}
		var templateID, templateSHA any
		if template != nil {
			templateID = template.Id
			templateSHA = template.Sha256
		}
		_, err := tx.Exec(ctx, "INSERT INTO execution_context_grants(id,tenant_id,execution_id,package_id,snapshot_sha256,input_sha256,template_id,template_sha256,generation_expires_at,review_expires_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", grantID, r.TenantId, r.ExecutionId, built.Package.Id, built.Package.SnapshotSha256, inputHash, templateID, templateSHA, built.Package.ExpiresAt.AsTime(), s.now().Add(24*time.Hour))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &pb.PreparedContext{ExecutionId: r.ExecutionId, GrantId: grantID, Package: built.Package, ApprovedTemplate: template}, nil
}
func (s *Service) GetLegalReview(ctx context.Context, r *pb.ReviewRequest) (*pb.ReviewResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := &pb.ReviewResponse{ExecutionId: r.ExecutionId}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var who, matter, cap string
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT principal_id,matter_id::text,capability_id,status,result,encode(digest(convert_to(result::text,'UTF8'),'sha256'),'hex') FROM ai_executions WHERE tenant_id=$1 AND id=$2", tenant, r.ExecutionId).Scan(&who, &matter, &cap, &reply.State, &raw, &reply.ArtifactSha256); err != nil {
			return err
		}
		permission := "draft.approve"
		if actor(ctx) == who {
			permission = cap
		}
		if err := access(ctx, tx, tenant, matter, actor(ctx), permission); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, matter, actor(ctx), "context.read"); err != nil {
			return err
		}
		var verified bool
		if err := tx.QueryRow(ctx, "SELECT verify_ai_execution_result($1,$2,$3::jsonb)", tenant, r.ExecutionId, raw).Scan(&verified); err != nil {
			return err
		}
		if !verified {
			return status.Error(codes.FailedPrecondition, "LEGAL_REVIEW_SOURCE_STALE")
		}
		var output struct {
			Output struct {
				Draft string `json:"draft"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &output) != nil {
			return status.Error(codes.DataLoss, "INVALID_DRAFT")
		}
		reply.Draft = output.Output.Draft
		var artifact map[string]any
		if json.Unmarshal(raw, &artifact) != nil {
			return status.Error(codes.DataLoss, "INVALID_DRAFT")
		}
		reply.Artifact, err = structpb.NewStruct(artifact)
		if err != nil {
			return status.Error(codes.DataLoss, "INVALID_DRAFT")
		}
		return nil
	})
	return reply, err
}
func (s *Service) DecideLegalReview(ctx context.Context, r *pb.ReviewRequest) (*pb.ReviewResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if !validID(r.ExecutionId) || len(r.ExpectedSha256) != 64 || (r.Decision != "approve" && r.Decision != "reject") {
		return nil, bad("INVALID_LEGAL_REVIEW")
	}
	reply := &pb.ReviewResponse{ExecutionId: r.ExecutionId, ArtifactSha256: r.ExpectedSha256}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT decide_legal_review($1,$2,$3,$4,$5)", tenant, r.ExecutionId, actor(ctx), r.ExpectedSha256, r.Decision).Scan(&reply.State); err != nil {
			return err
		}
		switch reply.State {
		case "completed", "rejected":
			return nil
		case "denied":
			return denied()
		default:
			return status.Error(codes.FailedPrecondition, "LEGAL_REVIEW_NOT_AUTHORIZED_OR_STALE")
		}
	})
	return reply, err
}
func (s *Service) GetReviewDeadline(ctx context.Context, r *pb.ExecutionRef) (*pb.ReviewDeadline, error) {
	reply := new(pb.ReviewDeadline)
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		var deadline time.Time
		if err := tx.QueryRow(ctx, "SELECT e.status,g.review_expires_at FROM ai_executions e JOIN execution_context_grants g ON g.tenant_id=e.tenant_id AND g.execution_id=e.id AND g.id=(e.result->'output'->>'grantId')::uuid WHERE e.tenant_id=$1 AND e.id=$2", r.TenantId, r.ExecutionId).Scan(&reply.State, &deadline); err != nil {
			return err
		}
		reply.Deadline = timestamppb.New(deadline)
		_, err := tx.Exec(ctx, "UPDATE integration_jobs SET state=CASE WHEN $3='awaiting_review' THEN 'running' WHEN $3='completed' THEN 'completed' ELSE 'rejected' END,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND kind='legal_review'", r.TenantId, r.ExecutionId, reply.State)
		return err
	})
	return reply, err
}
func (s *Service) ExpireReview(ctx context.Context, r *pb.ExecutionRef) (*pb.ReviewDeadline, error) {
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		var actorID string
		err := tx.QueryRow(ctx, "UPDATE ai_executions e SET status='rejected',error_code='LEGAL_REVIEW_EXPIRED',updated_at=now(),completed_at=now() FROM execution_context_grants g WHERE e.tenant_id=$1 AND e.id=$2 AND e.status='awaiting_review' AND g.tenant_id=e.tenant_id AND g.execution_id=e.id AND g.id=(e.result->'output'->>'grantId')::uuid AND g.review_expires_at<=now() RETURNING e.principal_id", r.TenantId, r.ExecutionId).Scan(&actorID)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code)VALUES($1,$2,'legal.review.expired',$3,'expired')", r.TenantId, actorID, r.ExecutionId); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES($1::uuid,'lawpilot.ai.review_decided.v1','1.0',$2::uuid,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','platform-api','type','lawpilot.ai.review_decided.v1','time',now(),'tenantId',$1::uuid::text,'schemaVersion','1.0','aggregateId',$2::uuid::text,'data',jsonb_build_object('executionId',$2::uuid::text,'state','rejected','decisionCode','review_expired')))`, r.TenantId, r.ExecutionId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetReviewDeadline(ctx, r)
}

func (s *Service) GetTemplate(ctx context.Context, r *pb.TemplateDecisionRequest) (*pb.TemplateResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := &pb.TemplateResponse{Template: &pb.ApprovedTemplate{Id: r.TemplateId}}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := tenantCapability(ctx, tx, tenant, actor(ctx), "template.manage"); err != nil {
			if err = tenantCapability(ctx, tx, tenant, actor(ctx), "template.approve"); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, "SELECT body_sha256,body,state FROM legal_template_versions WHERE tenant_id=$1 AND id=$2", tenant, r.TemplateId).Scan(&reply.Template.Sha256, &reply.Template.Body, &reply.State)
	})
	return reply, err
}
