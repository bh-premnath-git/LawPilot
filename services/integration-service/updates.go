package integrations

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"net/url"
	"strings"
)

func (s *Service) ProposeProviderUpdate(ctx context.Context, r *pb.ProposeProviderUpdateRequest) (*pb.ProviderProposalResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if !s.Writes {
		return nil, disabled()
	}
	if !validID(r.MatterId) || len(strings.TrimSpace(r.Title)) < 1 || len(r.Title) > 1000 {
		return nil, bad("INVALID_PROVIDER_UPDATE")
	}
	reply := new(pb.ProviderProposalResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := connectionAccess(ctx, tx, tenant, r.ConnectionId, who, "manage"); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, who, "provider.write", r.MatterId); err != nil {
			return err
		}
		if err := lock(ctx, tx, tenant, r.MatterId); err != nil {
			return err
		}
		hash := fingerprint("propose-provider-update", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		var external, etag string
		var revision int64
		if err = tx.QueryRow(ctx, "SELECT b.external_id,b.external_etag,m.revision FROM provider_sync_bindings b JOIN matters m ON m.tenant_id=b.tenant_id AND m.id=b.local_id JOIN provider_connections c ON c.tenant_id=b.tenant_id AND c.id=b.connection_id WHERE b.tenant_id=$1 AND b.connection_id=$2 AND b.resource_type='matter' AND b.local_id=$3 AND NOT b.deleted AND m.resource_owner='provider:'||c.provider_id", tenant, r.ConnectionId, r.MatterId).Scan(&external, &etag, &revision); err != nil {
			return err
		}
		id := uuid.NewString()
		raw, _ := json.Marshal([]any{tenant, r.ConnectionId, r.MatterId, external, etag, revision, r.Title, who})
		artifact := sha(raw)
		if _, err = tx.Exec(ctx, "INSERT INTO provider_update_proposals(id,tenant_id,connection_id,matter_id,external_id,expected_etag,expected_revision,title,artifact_sha256,proposed_by)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", id, tenant, r.ConnectionId, r.MatterId, external, etag, revision, r.Title, artifact, who); err != nil {
			return err
		}
		reply.OperationId = id
		reply.ArtifactSha256 = artifact
		reply.State = "pending"
		reply.Title = r.Title
		reply.MatterId = r.MatterId
		reply.ConnectionId = r.ConnectionId
		reply.ExpectedEtag = etag
		reply.ExpectedRevision = revision
		reply.ProposedBy = who
		if err = audit(ctx, tx, tenant, who, id, "provider.update.proposed", "requires-distinct-approval"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (s *Service) DecideProviderUpdate(ctx context.Context, r *pb.DecideProviderUpdateRequest) (*pb.ProviderProposalResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if !s.Writes {
		return nil, disabled()
	}
	if !validID(r.OperationId) || (r.Decision != "approve" && r.Decision != "reject") {
		return nil, bad("INVALID_PROVIDER_DECISION")
	}
	reply := new(pb.ProviderProposalResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, tenant, r.OperationId); err != nil {
			return err
		}
		var connectionID, matter, proposer, artifact, state string
		if err := tx.QueryRow(ctx, "SELECT connection_id::text,matter_id::text,proposed_by,artifact_sha256,state FROM provider_update_proposals WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, r.OperationId).Scan(&connectionID, &matter, &proposer, &artifact, &state); err != nil {
			return err
		}
		if _, err := connectionAccess(ctx, tx, tenant, connectionID, who, "approve"); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, who, "provider.approve", matter); err != nil {
			return err
		}
		if who == proposer {
			return status.Error(codes.PermissionDenied, "DISTINCT_APPROVER_REQUIRED")
		}
		if artifact != r.ArtifactSha256 {
			return status.Error(codes.Aborted, "PROVIDER_ARTIFACT_CHANGED")
		}
		hash := fingerprint("decide-provider-update", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		if state != "pending" {
			return status.Error(codes.FailedPrecondition, "PROVIDER_PROPOSAL_ALREADY_DECIDED")
		}
		state = "rejected"
		if r.Decision == "approve" {
			state = "approved"
			if err = job(ctx, tx, tenant, proposer, r.OperationId, "provider_update", r.OperationId); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE provider_update_proposals SET state=$3,reviewed_by=$4 WHERE tenant_id=$1 AND id=$2", tenant, r.OperationId, state, who); err != nil {
			return err
		}
		reply.OperationId = r.OperationId
		reply.ArtifactSha256 = artifact
		reply.State = state
		if err = audit(ctx, tx, tenant, who, r.OperationId, "provider.update.decided", state); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (p *Provider) ReadMatter(ctx context.Context, id, token string) (string, string, error) {
	target := p.Base + p.resourcePath("matter") + "/" + url.PathEscape(id)
	if p.ID == "clio-manage" {
		target = p.Base + "/matters/" + url.PathEscape(id) + ".json?fields=id,etag,description"
	}
	resp, err := p.request(ctx, "GET", target, token, "", nil)
	if err != nil {
		return "", "", unavailable("PROVIDER_RECONCILIATION_UNAVAILABLE")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", status.Error(codes.FailedPrecondition, "PROVIDER_RESOURCE_UNAVAILABLE")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return "", "", bad("PROVIDER_RESPONSE_TOO_LARGE")
	}
	var value struct {
		Data struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			ETag        string `json:"etag"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Data.ETag == "" {
		return "", "", bad("INVALID_PROVIDER_RESPONSE")
	}
	title := value.Data.Title
	if p.ID == "clio-manage" {
		title = value.Data.Description
	}
	return title, value.Data.ETag, nil
}
func (s *Service) processUpdate(ctx context.Context, tenant, jobID, id, who string) (*pb.JobResponse, error) {
	if !s.Writes {
		return nil, disabled()
	}
	var connectionID, matter, external, etag, title, reviewer, state string
	var expected int64
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, tenant, id); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT connection_id::text,matter_id::text,external_id,expected_etag,expected_revision,title,coalesce(reviewed_by,''),state FROM provider_update_proposals WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, id).Scan(&connectionID, &matter, &external, &etag, &expected, &title, &reviewer, &state); err != nil {
			return err
		}
		if _, err := connectionAccess(ctx, tx, tenant, connectionID, who, "manage"); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, who, "provider.write", matter); err != nil {
			return err
		}
		if _, err := connectionAccess(ctx, tx, tenant, connectionID, reviewer, "approve"); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, reviewer, "provider.approve", matter); err != nil {
			return err
		}
		if reviewer == who || reviewer == "" {
			return denied()
		}
		if state != "approved" && state != "inflight" && state != "completed" && state != "uncertain" && state != "conflict" {
			return status.Error(codes.FailedPrecondition, "PROVIDER_UPDATE_NOT_APPROVED")
		}
		if state == "approved" {
			var currentRevision int64
			var currentETag string
			if err := tx.QueryRow(ctx, "SELECT m.revision,b.external_etag FROM matters m JOIN provider_sync_bindings b ON b.tenant_id=m.tenant_id AND b.local_id=m.id AND b.resource_type='matter' WHERE m.tenant_id=$1 AND m.id=$2 AND b.connection_id=$3", tenant, matter, connectionID).Scan(&currentRevision, &currentETag); err != nil {
				return err
			}
			if expected != currentRevision || etag != currentETag {
				return status.Error(codes.FailedPrecondition, "PROVIDER_UPDATE_SOURCE_CHANGED")
			}
			_, err := tx.Exec(ctx, "UPDATE provider_update_proposals SET state='inflight' WHERE tenant_id=$1 AND id=$2", tenant, id)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if state == "completed" || state == "uncertain" || state == "conflict" {
		return &pb.JobResponse{Id: jobID, State: map[string]string{"completed": "completed", "uncertain": "uncertain", "conflict": "failed"}[state]}, nil
	}
	p, token, err := s.token(ctx, tenant, connectionID, who, "manage")
	if err != nil {
		return nil, err
	}
	outcome := "uncertain"
	// A crash/lost response after inflight is recorded permits only a GET-based
	// reconciliation. It can never blindly repeat an uncertain PATCH.
	if state == "approved" {
		outcome, err = p.MatterUpdate(ctx, external, title, etag, token)
		if err != nil {
			return nil, err
		}
	}
	currentTitle, currentETag, readErr := p.ReadMatter(ctx, external, token)
	if readErr == nil && currentTitle == title {
		outcome = "completed"
	} else if outcome == "completed" {
		outcome = "uncertain"
	}
	jobState := outcome
	code := ""
	if outcome == "conflict" {
		jobState = "failed"
		code = "PROVIDER_ETAG_CONFLICT"
	}
	if outcome == "uncertain" {
		code = "PROVIDER_WRITE_UNCERTAIN_REQUIRES_REVIEW"
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE provider_update_proposals SET state=$3 WHERE tenant_id=$1 AND id=$2", tenant, id, outcome); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE integration_jobs SET state=$3,error_code=NULLIF($4,''),updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, jobID, jobState, code); err != nil {
			return err
		}
		if outcome == "completed" {
			if _, err := tx.Exec(ctx, "UPDATE matters SET title=$3,revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, matter, title); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE provider_sync_bindings SET external_etag=$4,last_synced_at=now() WHERE tenant_id=$1 AND connection_id=$2 AND resource_type='matter' AND local_id=$3", tenant, connectionID, matter, currentETag); err != nil {
				return err
			}
		}
		if outcome == "completed" {
			if err := changed(ctx, tx, tenant, matter, "matters", "matter.updated.v1"); err != nil {
				return err
			}
		}
		return audit(ctx, tx, tenant, who, id, "provider.update.result", outcome)
	})
	return &pb.JobResponse{Id: jobID, State: jobState, ErrorCode: code}, err
}

func (s *Service) GetProviderProposal(ctx context.Context, r *pb.ProviderProposalRequest) (*pb.ProviderProposalResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := &pb.ProviderProposalResponse{OperationId: r.OperationId}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT connection_id::text,matter_id::text,title,expected_etag,expected_revision,artifact_sha256,proposed_by,state FROM provider_update_proposals WHERE tenant_id=$1 AND id=$2", tenant, r.OperationId).Scan(&reply.ConnectionId, &reply.MatterId, &reply.Title, &reply.ExpectedEtag, &reply.ExpectedRevision, &reply.ArtifactSha256, &reply.ProposedBy, &reply.State); err != nil {
			return err
		}
		permission := "approve"
		if reply.ProposedBy == actor(ctx) {
			permission = "manage"
		}
		if _, err := connectionAccess(ctx, tx, tenant, reply.ConnectionId, actor(ctx), permission); err != nil {
			return err
		}
		return access(ctx, tx, tenant, actor(ctx), "provider."+permission, reply.MatterId)
	})
	return reply, err
}
