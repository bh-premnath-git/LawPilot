package contextengine

import (
	"context"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"testing"
)

func TestConfiguredDraftRequiresDistinctReviewAndCurrentSources(t *testing.T) {
	f := fixtureFor(t)
	ctx := context.Background()
	doc := f.ingest(t, "Lease agreement. Rent is due monthly.")
	reviewer := uuid.NewString()
	id := uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.admin.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO memberships VALUES($1,$2,'active')", f.tenant, reviewer)
	for _, cap := range []string{"draft.approve", "context.read"} {
		exec("INSERT INTO principal_capabilities VALUES($1,$2,$3)", f.tenant, reviewer, cap)
	}
	exec("INSERT INTO matter_grants VALUES($1,$2,$3,'matter.read')", f.tenant, f.matter, reviewer)
	exec("INSERT INTO principal_capabilities VALUES($1,$2,'draft.approve')", f.tenant, f.who)
	exec("INSERT INTO ai_executions(id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,matter_id,status,request_input,workflow_id)VALUES($1,$2,$3,$4,$5,'matter.qa.v1',$6,'running','{\"question\":\"lease\"}',$7)", id, f.tenant, f.who, uuid.NewString(), digest("synthetic"), f.matter, "test:"+id)
	prepared, err := f.s.PrepareExecutionContext(ctx, &pb.ExecutionRef{TenantId: f.tenant, ExecutionId: id})
	if err != nil {
		t.Fatal(err)
	}
	c := prepared.Package.Sources[0].Citation
	raw, _ := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(c)
	var citation any
	json.Unmarshal(raw, &citation)
	result := map[string]any{"executionId": id, "agentId": "case-qa-agent", "agentVersion": "0.1.0", "status": "completed", "output": map[string]any{"kind": "legal_draft.v1", "draft": "Unapproved synthetic draft.", "claims": []any{map[string]any{"citation": citation, "quote": "Lease agreement."}}, "limitations": []string{"Attorney review required"}, "grantId": prepared.GrantId, "modelProfile": digest("fake-model")}, "evidence": []any{map[string]any{"documentId": c.DocumentId, "documentVersionId": c.VersionId, "passageId": c.PassageId, "contentSha256": c.ContentSha256}}, "proposedActions": []any{}, "requiresAttorneyReview": true}
	bytes, _ := json.Marshal(result)
	err = f.s.tx(ctx, f.tenant, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, "SELECT verify_ai_execution_result($1,$2,$3::jsonb)", f.tenant, id, bytes).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			t.Error("valid grant rejected")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	exec("UPDATE ai_executions SET result=$3,status='awaiting_review' WHERE tenant_id=$1 AND id=$2", f.tenant, id, bytes)
	reviewerCtx := context.WithValue(ctx, principalKey{}, reviewer)
	preview, err := f.s.GetLegalReview(reviewerCtx, &pb.ReviewRequest{Scope: f.scope(), ExecutionId: id})
	if err != nil || preview.Artifact == nil {
		t.Fatal("review preview", err)
	}
	if _, err = f.s.DecideLegalReview(f.ctx(), &pb.ReviewRequest{Scope: f.scope(), ExecutionId: id, ExpectedSha256: preview.ArtifactSha256, Decision: "approve"}); err == nil {
		t.Fatal("self approval accepted")
	}
	if _, err = f.s.DecideLegalReview(reviewerCtx, &pb.ReviewRequest{Scope: f.scope(), ExecutionId: id, ExpectedSha256: digest("wrong"), Decision: "approve"}); err == nil {
		t.Fatal("wrong artifact accepted")
	}
	decision, err := f.s.DecideLegalReview(reviewerCtx, &pb.ReviewRequest{Scope: f.scope(), ExecutionId: id, ExpectedSha256: preview.ArtifactSha256, Decision: "approve"})
	if err != nil || decision.State != "completed" {
		t.Fatal("distinct approval", err)
	}
	if _, err = f.s.IngestText(f.ctx(), &pb.IngestTextRequest{Scope: f.scope(), MatterId: f.matter, DocumentId: doc.Id, ExpectedRevision: 1, Title: "Changed", Text: "Lease superseded.", IdempotencyKey: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.GetLegalReview(reviewerCtx, &pb.ReviewRequest{Scope: f.scope(), ExecutionId: id}); err == nil {
		t.Fatal("superseded source released approved result")
	}
}
func TestProcessedFileRequiresScanProofAndPreservesPages(t *testing.T) {
	f := fixtureFor(t)
	ctx := context.Background()
	id := uuid.NewString()
	text := "Lease page one.\n\f\nLease page two."
	if _, err := f.admin.Exec(ctx, "INSERT INTO file_ingestions(id,tenant_id,matter_id,principal_id,filename,media_type,size_bytes,source_sha256,quarantine_key,clean_key,expires_at,state,scan_status,parsed_sha256)VALUES($1,$2,$3,$4,'lease.pdf','application/pdf',10,$5,'q','c',now()+interval '1 hour','processing','pending',$6)", id, f.tenant, f.matter, f.who, digest("original"), digest(text)); err != nil {
		t.Fatal(err)
	}
	r := &pb.IngestProcessedRequest{TenantId: f.tenant, FileId: id, ParserVersion: "isolated-text.v1", Pages: []*pb.ProcessedPage{{Number: 1, Text: "Lease page one."}, {Number: 2, Text: "Lease page two."}}}
	if _, err := f.s.IngestProcessed(ctx, r); err == nil {
		t.Fatal("unscanned file indexed")
	}
	f.admin.Exec(ctx, "UPDATE file_ingestions SET scan_status='clean' WHERE id=$1", id)
	doc, err := f.s.IngestProcessed(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := f.s.BuildContext(f.ctx(), &pb.BuildContextRequest{Scope: f.scope(), MatterId: f.matter, RecipeId: "document.summary.v1", DocumentId: doc.Document.Id})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Package.Sources) != 2 || pkg.Package.Sources[1].Citation.GetPageNumber() != 2 {
		t.Fatal("page provenance lost")
	}
	var proof string
	if err = f.admin.QueryRow(ctx, "SELECT source_file_sha256 FROM document_versions WHERE id=$1", doc.Document.VersionId).Scan(&proof); err != nil || proof != digest("original") {
		t.Fatal("original proof", err)
	}
}

func TestTemplateApprovalIsDistinctAndRevocationBlocksGeneration(t *testing.T) {
	f := fixtureFor(t)
	ctx := context.Background()
	f.ingest(t, "Lease evidence.")
	reviewer := uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.admin.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO memberships VALUES($1,$2,'active')", f.tenant, reviewer)
	for _, cap := range []string{"template.manage", "template.approve", "legal.draft.v1"} {
		exec("INSERT INTO principal_capabilities VALUES($1,$2,$3)", f.tenant, f.who, cap)
	}
	exec("INSERT INTO principal_capabilities VALUES($1,$2,'template.approve')", f.tenant, reviewer)
	template, err := f.s.CreateTemplate(f.ctx(), &pb.TemplateRequest{Scope: f.scope(), Name: "Lease memo", Version: 1, Body: "State source-supported facts and limitations.", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	decision := &pb.TemplateDecisionRequest{Scope: f.scope(), TemplateId: template.Template.Id, ExpectedSha256: template.Template.Sha256, Decision: "approve"}
	if _, err = f.s.DecideTemplate(f.ctx(), decision); err == nil {
		t.Fatal("self approval accepted")
	}
	reviewCtx := context.WithValue(ctx, principalKey{}, reviewer)
	if _, err = f.s.DecideTemplate(reviewCtx, decision); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	input, _ := json.Marshal(map[string]string{"instructions": "lease", "templateId": template.Template.Id})
	exec("INSERT INTO ai_executions(id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,matter_id,status,request_input,workflow_id)VALUES($1,$2,$3,$4,$5,'legal.draft.v1',$6,'running',$7,$8)", id, f.tenant, f.who, uuid.NewString(), digest("input"), f.matter, input, "test:"+id)
	ref := &pb.ExecutionRef{TenantId: f.tenant, ExecutionId: id}
	prepared, err := f.s.PrepareExecutionContext(ctx, ref)
	if err != nil || prepared.ApprovedTemplate.Id != template.Template.Id {
		t.Fatal("approved template not bound", err)
	}
	decision.Decision = "revoke"
	if _, err = f.s.DecideTemplate(reviewCtx, decision); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PrepareExecutionContext(ctx, ref); err == nil {
		t.Fatal("revoked template used")
	}
}
