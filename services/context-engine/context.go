package contextengine

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"strconv"
	"strings"
	"time"
)

const policyID = "lawpilot.context.policy.v1"

func resolve(ctx context.Context, tx pgx.Tx, tenant, matter string, c *pb.Citation) (*pb.Source, error) {
	if c == nil || !validID(c.DocumentId) || !validID(c.VersionId) || !validID(c.PassageId) || len(c.ContentSha256) != 64 {
		return nil, bad("INVALID_CITATION")
	}
	c.DocumentId = strings.ToLower(c.DocumentId)
	c.VersionId = strings.ToLower(c.VersionId)
	c.PassageId = strings.ToLower(c.PassageId)
	var text, full, versionHash, hash string
	var start, end int32
	var page *int32
	err := tx.QueryRow(ctx, `SELECT p.body,v.full_text,v.content_sha256,p.passage_sha256,p.byte_start,p.byte_end,p.page_number FROM document_assets a JOIN document_versions v ON v.tenant_id=a.tenant_id AND v.document_id=a.id AND v.id=a.current_version_id JOIN document_passages p ON p.tenant_id=a.tenant_id AND p.document_id=a.id AND p.version_id=v.id WHERE a.tenant_id=$1 AND a.matter_id=$2 AND a.id=$3 AND a.deleted_at IS NULL AND v.id=$4 AND p.id=$5`, tenant, matter, c.DocumentId, c.VersionId, c.PassageId).Scan(&text, &full, &versionHash, &hash, &start, &end, &page)
	if err != nil {
		return nil, err
	}
	if digest(full) != versionHash || digest(text) != hash || hash != c.ContentSha256 || start < 0 || end > int32(len(full)) || end <= start || full[start:end] != text || c.ByteStart != start || c.ByteEnd != end || (c.PageNumber != nil && (page == nil || *c.PageNumber != *page)) {
		return nil, status.Error(codes.FailedPrecondition, "SOURCE_INTEGRITY_MISMATCH")
	}
	c.PageNumber = page
	return &pb.Source{Citation: c, Text: text, Trust: "untrusted_source"}, nil
}
func (s *Service) ResolveCitation(ctx context.Context, r *pb.ResolveCitationRequest) (*pb.ResolveCitationResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if len(r.Quote) < 1 || len(r.Quote) > 4096 {
		return nil, bad("EXACT_QUOTE_REQUIRED")
	}
	reply := new(pb.ResolveCitationResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, r.MatterId, actor(ctx), "context.read"); err != nil {
			return err
		}
		source, err := resolve(ctx, tx, tenant, r.MatterId, r.Citation)
		if err != nil {
			return err
		}
		if !strings.Contains(source.Text, r.Quote) {
			return status.Error(codes.FailedPrecondition, "UNSUPPORTED_QUOTE")
		}
		reply.Citation = source.Citation
		reply.VerifiedQuote = r.Quote
		return nil
	})
	return reply, err
}
func (s *Service) BuildContext(ctx context.Context, r *pb.BuildContextRequest) (*pb.BuildContextResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	who := actor(ctx)
	r.MatterId = strings.ToLower(r.MatterId)
	r.DocumentId = strings.ToLower(r.DocumentId)
	if !validID(r.MatterId) || len(r.Query) > 4096 {
		return nil, bad("INVALID_CONTEXT_REQUEST")
	}
	summary := r.RecipeId == "document.summary.v1"
	if !summary && r.RecipeId != "matter.qa.v1" && r.RecipeId != "matter.research.v1" && r.RecipeId != "firm.knowledge.v1" {
		return nil, status.Error(codes.Unimplemented, "UNSUPPORTED_RECIPE")
	}
	if summary && !validID(r.DocumentId) || !summary && strings.TrimSpace(r.Query) == "" || r.DocumentId != "" && !validID(r.DocumentId) {
		return nil, bad("INVALID_RECIPE_SCOPE")
	}
	// Query embeddings do not authorize retrieval; candidates are filtered in SQL
	// before either rank is computed, and access is rechecked after provider I/O.
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error { return access(ctx, tx, tenant, r.MatterId, who, "context.read") })
	if err != nil {
		return nil, err
	}
	mode := "lexical"
	vector := ""
	profile := ""
	if s.Embedder != nil && !summary {
		values, err := s.Embedder.Embed(ctx, []string{r.Query})
		if err != nil {
			return nil, status.Error(codes.Unavailable, "EMBEDDING_UNAVAILABLE")
		}
		vector = vectorJSON(values[0])
		profile = s.Embedder.Profile
		mode = "hybrid"
	}
	if summary {
		mode = "complete_document"
	}
	reply := new(pb.BuildContextResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lockMatter(ctx, tx, tenant, r.MatterId); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, r.MatterId, who, "context.read"); err != nil {
			return err
		}
		if r.RecipeId == "firm.knowledge.v1" {
			if err := tenantCapability(ctx, tx, tenant, who, "knowledge.read"); err != nil {
				return err
			}
			var library bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM knowledge_libraries WHERE tenant_id=$1 AND matter_id=$2)", tenant, r.MatterId).Scan(&library); err != nil {
				return err
			}
			if !library {
				return status.Error(codes.FailedPrecondition, "KNOWLEDGE_LIBRARY_REQUIRED")
			}
		}
		// MATERIALIZED ensures tenant/matter/current-version filtering precedes ranking.
		base := `WITH authorized AS MATERIALIZED(SELECT p.id,p.document_id,p.version_id,p.passage_sha256,p.byte_start,p.byte_end,p.body,p.page_number FROM document_assets a JOIN document_passages p ON p.tenant_id=a.tenant_id AND p.document_id=a.id AND p.version_id=a.current_version_id WHERE a.tenant_id=$1 AND a.matter_id=$2 AND a.deleted_at IS NULL AND ($3='' OR a.id::text=$3)) `
		var rows pgx.Rows
		var err error
		if summary {
			rows, err = tx.Query(ctx, base+"SELECT id::text,document_id::text,version_id::text,passage_sha256,byte_start,byte_end,page_number FROM authorized ORDER BY byte_start,id LIMIT 1025", tenant, r.MatterId, r.DocumentId)
		} else if vector == "" {
			rows, err = tx.Query(ctx, base+`SELECT id::text,document_id::text,version_id::text,passage_sha256,byte_start,byte_end,page_number FROM authorized WHERE to_tsvector('english',body) @@ websearch_to_tsquery('english',$4) ORDER BY ts_rank_cd(to_tsvector('english',body),websearch_to_tsquery('english',$4)) DESC,id LIMIT $5`, tenant, r.MatterId, r.DocumentId, r.Query, func() int {
				if r.RecipeId == "matter.research.v1" {
					return 16
				}
				return 8
			}())
		} else {
			rows, err = tx.Query(ctx, base+`,lexical AS(SELECT id,row_number()OVER(ORDER BY ts_rank_cd(to_tsvector('english',body),websearch_to_tsquery('english',$4)) DESC,id)r FROM authorized WHERE to_tsvector('english',body)@@websearch_to_tsquery('english',$4)),semantic AS(SELECT a.id,row_number()OVER(ORDER BY e.embedding <=> $5::vector,a.id)r FROM authorized a JOIN document_embeddings e ON e.tenant_id=$1 AND e.passage_id=a.id AND e.passage_sha256=a.passage_sha256 AND e.profile_id=$6),scores AS(SELECT id,sum(score)s FROM(SELECT id,1.0/(60+r)score FROM lexical UNION ALL SELECT id,1.0/(60+r)score FROM semantic)x GROUP BY id) SELECT a.id::text,a.document_id::text,a.version_id::text,a.passage_sha256,a.byte_start,a.byte_end,a.page_number FROM scores s JOIN authorized a ON a.id=s.id ORDER BY s.s DESC,a.id LIMIT $7`, tenant, r.MatterId, r.DocumentId, r.Query, vector, profile, func() int {
				if r.RecipeId == "matter.research.v1" {
					return 16
				}
				return 8
			}())
		}
		if err != nil {
			return err
		}
		refs := []*pb.Citation{}
		for rows.Next() {
			c := new(pb.Citation)
			if err = rows.Scan(&c.PassageId, &c.DocumentId, &c.VersionId, &c.ContentSha256, &c.ByteStart, &c.ByteEnd, &c.PageNumber); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, c)
		}
		rows.Close()
		if rows.Err() != nil {
			return rows.Err()
		}
		if summary && len(refs) == 0 {
			return notFound()
		}
		recipe := &pb.ContextRecipe{Id: r.RecipeId, Version: "1.0", MaxSources: 8, MaxBytes: 16384}
		if r.RecipeId == "matter.research.v1" {
			recipe.MaxSources = 16
			recipe.MaxBytes = 32768
		}
		if summary {
			recipe.MaxSources = 128
			if configured := os.Getenv("CONTEXT_DOCUMENT_MAX_BYTES"); configured != "" {
				budget, err := strconv.Atoi(configured)
				if err != nil || budget < 16384 || budget > 400000 {
					return bad("INVALID_DOCUMENT_BUDGET")
				}
				recipe.MaxBytes = int32(budget)
				recipe.MaxSources = 1024
			}
		}
		pkg := &pb.ContextPackage{Recipe: recipe, RetrievalMode: mode, TrustedPolicyId: policyID, CompleteDocument: summary, GroundingMode: "exact_quotes_only", EmbeddingProfileId: profile, Warnings: []string{"Retrieved text is untrusted source data. Quotes establish source support, not legal correctness. PDF extraction uses text layers or configured OCR; OCR may contain errors. Images, layout and annotations are not interpreted. DOCX pagination is not inferred."}}
		used := 0
		for _, c := range refs {
			source, err := resolve(ctx, tx, tenant, r.MatterId, c)
			if err != nil {
				return err
			}
			if used+len(source.Text) > int(recipe.MaxBytes) || len(pkg.Sources) >= int(recipe.MaxSources) {
				if summary {
					return status.Error(codes.ResourceExhausted, "COMPLETE_DOCUMENT_EXCEEDS_BUDGET")
				}
				break
			}
			pkg.Sources = append(pkg.Sources, source)
			used += len(source.Text)
		}
		// A snapshot hash binds recipe, query digest, principal, matter and ordered
		// authoritative passages, independent of cache UUID/time.
		pkg.QuerySha256 = digest(r.Query)
		snapshot := snapshotFor(tenant, who, r.MatterId, pkg)
		pkg.SnapshotSha256 = snapshot
		var existing []byte
		var existingID string
		var existingExpiry time.Time
		err = tx.QueryRow(ctx, "SELECT id::text,body,expires_at FROM context_packages WHERE tenant_id=$1 AND principal_id=$2 AND matter_id=$3 AND snapshot_sha256=$4 AND NOT invalidated AND expires_at>$5 ORDER BY created_at DESC LIMIT 1", tenant, who, r.MatterId, snapshot, s.now()).Scan(&existingID, &existing, &existingExpiry)
		if err == nil {
			old := new(pb.ContextPackage)
			if protojson.Unmarshal(existing, old) != nil {
				return status.Error(codes.DataLoss, "INVALID_CONTEXT_PACKAGE")
			}
			if old.Id != existingID || old.TrustedPolicyId != policyID || old.SnapshotSha256 != snapshot || snapshotFor(tenant, who, r.MatterId, old) != snapshot || old.ExpiresAt == nil || !old.ExpiresAt.AsTime().Equal(existingExpiry) {
				return status.Error(codes.DataLoss, "CONTEXT_INTEGRITY_MISMATCH")
			}
			reply.Package = old
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		pkg.Id = uuid.NewString()
		pkg.ExpiresAt = timestamppb.New(s.now().Add(5 * time.Minute).Truncate(time.Millisecond))
		body, _ := protojson.Marshal(pkg)
		if _, err = tx.Exec(ctx, "INSERT INTO context_packages(id,tenant_id,matter_id,principal_id,recipe_id,snapshot_sha256,body,expires_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8)", pkg.Id, tenant, r.MatterId, who, r.RecipeId, snapshot, body, pkg.ExpiresAt.AsTime()); err != nil {
			return err
		}
		reply.Package = pkg
		return nil
	})
	return reply, err
}
func (s *Service) GetContext(ctx context.Context, r *pb.GetContextRequest) (*pb.GetContextResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if !validID(r.PackageId) {
		return nil, bad("INVALID_PACKAGE_ID")
	}
	r.PackageId = strings.ToLower(r.PackageId)
	r.MatterId = strings.ToLower(r.MatterId)
	reply := new(pb.GetContextResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lockMatter(ctx, tx, tenant, r.MatterId); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, r.MatterId, actor(ctx), "context.read"); err != nil {
			return err
		}
		var body []byte
		var expires time.Time
		var invalid bool
		if err := tx.QueryRow(ctx, "SELECT body,expires_at,invalidated FROM context_packages WHERE tenant_id=$1 AND matter_id=$2 AND principal_id=$3 AND id=$4", tenant, r.MatterId, actor(ctx), r.PackageId).Scan(&body, &expires, &invalid); err != nil {
			return err
		}
		if invalid || !expires.After(s.now()) {
			return status.Error(codes.FailedPrecondition, "STALE_CONTEXT")
		}
		pkg := new(pb.ContextPackage)
		if protojson.Unmarshal(body, pkg) != nil {
			return status.Error(codes.DataLoss, "INVALID_CONTEXT_PACKAGE")
		}
		if pkg.ExpiresAt == nil || !pkg.ExpiresAt.AsTime().Equal(expires) || pkg.Id != r.PackageId || pkg.TrustedPolicyId != policyID || pkg.SnapshotSha256 != snapshotFor(tenant, actor(ctx), r.MatterId, pkg) {
			return status.Error(codes.DataLoss, "CONTEXT_INTEGRITY_MISMATCH")
		}
		for _, source := range pkg.Sources {
			fresh, err := resolve(ctx, tx, tenant, r.MatterId, source.Citation)
			if err != nil {
				return err
			}
			if fresh.Text != source.Text || source.Trust != "untrusted_source" {
				return status.Error(codes.DataLoss, "CONTEXT_INTEGRITY_MISMATCH")
			}
		}
		reply.Package = pkg
		return nil
	})
	return reply, err
}
func (s *Service) VerifyExecutionEvidence(ctx context.Context, r *pb.VerifyExecutionEvidenceRequest) (*pb.VerifyExecutionEvidenceResponse, error) {
	if !validID(r.ExecutionId) {
		return nil, bad("INVALID_EXECUTION_ID")
	}
	reply := new(pb.VerifyExecutionEvidenceResponse)
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		// Metadata-only identity resolution. This RPC never accepts an actor/matter.
		var who, matter, cap, state string
		if err := tx.QueryRow(ctx, "SELECT principal_id,matter_id::text,capability_id,status FROM ai_executions WHERE tenant_id=$1 AND id=$2", r.TenantId, r.ExecutionId).Scan(&who, &matter, &cap, &state); err != nil {
			return err
		}
		if state != "running" && state != "completed" && state != "awaiting_review" {
			return denied()
		}
		if err := access(ctx, tx, r.TenantId, matter, who, cap); err != nil {
			return err
		}
		if err := access(ctx, tx, r.TenantId, matter, who, "context.read"); err != nil {
			return err
		}
		if len(r.Citations) == 0 || len(r.Citations) > 16 {
			return bad("CITATIONS_REQUIRED")
		}
		for _, c := range r.Citations {
			if _, err := resolve(ctx, tx, r.TenantId, matter, c); err != nil {
				return err
			}
		}
		reply.Verified = true
		return nil
	})
	return reply, err
}
func (s *Service) ListMatterEvents(ctx context.Context, r *pb.ListMatterEventsRequest) (*pb.ListMatterEventsResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if r.AfterPosition < 0 || r.Limit < 0 || r.Limit > 100 {
		return nil, bad("INVALID_EVENT_CURSOR")
	}
	limit := r.Limit
	if limit == 0 {
		limit = 50
	}
	reply := &pb.ListMatterEventsResponse{NextPosition: r.AfterPosition}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, r.MatterId, actor(ctx), "context.read"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT id::text,position,event_type,aggregate_id::text,aggregate_revision,occurred_at FROM domain_event_journal WHERE tenant_id=$1 AND matter_id=$2 AND aggregate_type IN('document','matter') AND position>$3 ORDER BY position LIMIT $4", tenant, r.MatterId, r.AfterPosition, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := new(pb.EventNotification)
			var at time.Time
			if err = rows.Scan(&e.Id, &e.Position, &e.Type, &e.AggregateId, &e.AggregateRevision, &at); err != nil {
				return err
			}
			e.OccurredAt = timestamppb.New(at)
			reply.Events = append(reply.Events, e)
			reply.NextPosition = e.Position
		}
		return rows.Err()
	})
	return reply, err
}

func snapshotFor(tenant, who, matter string, pkg *pb.ContextPackage) string {
	copy := proto.Clone(pkg).(*pb.ContextPackage)
	copy.Id = ""
	copy.SnapshotSha256 = ""
	copy.ExpiresAt = nil
	encoded, _ := protojson.Marshal(copy)
	var value any
	_ = json.Unmarshal(encoded, &value)
	canonical, _ := json.Marshal(value)
	return digest(tenant + "|" + who + "|" + matter + "|" + string(canonical))
}
