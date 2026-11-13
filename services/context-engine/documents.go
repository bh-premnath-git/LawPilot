package contextengine

import (
	"context"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"unicode/utf8"
)

type chunk struct {
	Start, End int
	Text       string
	Page       *int32
}

func chunks(text string) []chunk {
	positions := []int{}
	for i := range text {
		positions = append(positions, i)
	}
	positions = append(positions, len(text))
	out := []chunk{}
	for i := 0; i < len(positions)-1; i += 448 {
		end := i + 512
		if end >= len(positions) {
			end = len(positions) - 1
		}
		out = append(out, chunk{positions[i], positions[end], text[positions[i]:positions[end]], nil})
		if end == len(positions)-1 {
			break
		}
	}
	return out
}
func (s *Service) IngestText(ctx context.Context, r *pb.IngestTextRequest) (*pb.IngestTextResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	who := actor(ctx)
	provenance, processed := ctx.Value(processedKey{}).(fileProvenance)
	limit := 24576
	if processed {
		limit = 262144
	}
	if !validID(r.MatterId) || r.DocumentId != "" && !validID(r.DocumentId) || r.DocumentId == "" && r.ExpectedRevision != 0 || len(strings.TrimSpace(r.Title)) < 1 || len(r.Title) > 256 || len(r.Text) < 1 || len(r.Text) > limit || strings.TrimSpace(r.Text) == "" || !utf8.ValidString(r.Text) || strings.ContainsRune(r.Text, 0) {
		return nil, bad("INVALID_NATIVE_TEXT")
	}
	reply := new(pb.IngestTextResponse)
	hash := commandHash("ingest-text", r)
	hit := false
	// Authorize before sending any text to a configured embedding provider.
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, r.MatterId, who, "document.ingest"); err != nil {
			return err
		}
		var err error
		hit, err = cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		return err
	})
	if err != nil || hit {
		return reply, err
	}
	parts := chunks(r.Text)
	if processed {
		parts = provenance.Parts
	}
	var vectors [][]float64
	if s.Embedder != nil {
		for start := 0; start < len(parts); start += 64 {
			end := start + 64
			if end > len(parts) {
				end = len(parts)
			}
			input := make([]string, end-start)
			for i, c := range parts[start:end] {
				input[i] = c.Text
			}
			batch, e := s.Embedder.Embed(ctx, input)
			if e != nil {
				return nil, status.Error(codes.Unavailable, "EMBEDDING_UNAVAILABLE")
			}
			vectors = append(vectors, batch...)
		}
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lockMatter(ctx, tx, tenant, r.MatterId); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, r.MatterId, who, "document.ingest"); err != nil {
			return err
		}
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		if processed {
			var ok bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM file_ingestions WHERE tenant_id=$1 AND id=$2 AND principal_id=$3 AND matter_id=$4 AND state='processing' AND scan_status='clean' AND source_sha256=$5 AND parsed_sha256=$6)", tenant, provenance.ID, who, r.MatterId, provenance.SHA, digest(r.Text)).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return status.Error(codes.FailedPrecondition, "FILE_STATE_CHANGED")
			}
		}
		id := r.DocumentId
		revision := int64(0)
		if id == "" {
			id = uuid.NewString()
			_, err = tx.Exec(ctx, "INSERT INTO document_assets(id,tenant_id,matter_id,storage_ref,content_sha256,version,scan_status,title)VALUES($1,$2,$3,$4,$5,1,'text_only',$6)", id, tenant, r.MatterId, "native-text:"+id, digest(r.Text), r.Title)
		} else {
			err = tx.QueryRow(ctx, "SELECT revision FROM document_assets WHERE tenant_id=$1 AND matter_id=$2 AND id=$3 AND deleted_at IS NULL FOR UPDATE", tenant, r.MatterId, id).Scan(&revision)
		}
		if err != nil {
			return err
		}
		if revision != r.ExpectedRevision {
			return status.Error(codes.Aborted, "STALE_DOCUMENT_REVISION")
		}
		revision++
		version := uuid.NewString()
		var sourceFile any
		var sourceSHA any
		parserVersion := "native-text.v1"
		if processed {
			sourceFile = provenance.ID
			sourceSHA = provenance.SHA
			parserVersion = provenance.Parser
		}
		if _, err = tx.Exec(ctx, "INSERT INTO document_versions(id,tenant_id,document_id,revision,content_sha256,full_text,created_by,source_file_id,source_file_sha256,parser_version)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", version, tenant, id, revision, digest(r.Text), r.Text, who, sourceFile, sourceSHA, parserVersion); err != nil {
			return err
		}
		for i, c := range parts {
			passage := uuid.NewString()
			sha := digest(c.Text)
			if _, err = tx.Exec(ctx, "INSERT INTO document_passages(id,tenant_id,document_id,source_sha256,text_ref,version_id,body,byte_start,byte_end,passage_sha256,page_number)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", passage, tenant, id, digest(r.Text), "native-text:"+passage, version, c.Text, c.Start, c.End, sha, c.Page); err != nil {
				return err
			}
			if s.Embedder != nil {
				if _, err = tx.Exec(ctx, "INSERT INTO document_embeddings VALUES($1,$2,$3,$4,$5::vector)", tenant, passage, sha, s.Embedder.Profile, vectorJSON(vectors[i])); err != nil {
					return err
				}
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE document_assets SET title=$4,current_version_id=$5,revision=$6::bigint,version=($6::bigint)::integer,content_sha256=$7 WHERE tenant_id=$1 AND matter_id=$2 AND id=$3", tenant, r.MatterId, id, r.Title, version, revision, digest(r.Text)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE context_packages SET invalidated=true WHERE tenant_id=$1 AND matter_id=$2", tenant, r.MatterId); err != nil {
			return err
		}
		mode := "lexical"
		if s.Embedder != nil {
			mode = "hybrid"
		}
		reply.Document = &pb.Document{Id: id, MatterId: r.MatterId, VersionId: version, Revision: revision, ContentSha256: digest(r.Text), State: "active", PassageCount: int32(len(parts)), IndexingMode: mode}
		if processed {
			if _, err = tx.Exec(ctx, "UPDATE file_ingestions SET state='completed',document_id=$3,version_id=$4 WHERE tenant_id=$1 AND id=$2", tenant, provenance.ID, id, version); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "UPDATE document_assets SET scan_status='clean' WHERE tenant_id=$1 AND id=$2", tenant, id); err != nil {
				return err
			}
		}
		if err = emit(ctx, tx, tenant, who, id, "document.version.created.v1", revision); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (s *Service) DeleteDocument(ctx context.Context, r *pb.DeleteDocumentRequest) (*pb.DeleteDocumentResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	who := actor(ctx)
	if !validID(r.DocumentId) || !validID(r.MatterId) {
		return nil, bad("INVALID_DOCUMENT")
	}
	reply := new(pb.DeleteDocumentResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lockMatter(ctx, tx, tenant, r.MatterId); err != nil {
			return err
		}
		if err := access(ctx, tx, tenant, r.MatterId, who, "document.delete"); err != nil {
			return err
		}
		hash := commandHash("delete-document", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		var revision int64
		err = tx.QueryRow(ctx, "SELECT revision FROM document_assets WHERE tenant_id=$1 AND matter_id=$2 AND id=$3 AND deleted_at IS NULL FOR UPDATE", tenant, r.MatterId, r.DocumentId).Scan(&revision)
		if err != nil {
			return err
		}
		if revision != r.ExpectedRevision {
			return status.Error(codes.Aborted, "STALE_DOCUMENT_REVISION")
		}
		if _, err = tx.Exec(ctx, "UPDATE document_assets SET deleted_at=now(),revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, r.DocumentId); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE context_packages SET invalidated=true WHERE tenant_id=$1 AND matter_id=$2", tenant, r.MatterId); err != nil {
			return err
		}
		reply.Document = &pb.Document{Id: r.DocumentId, MatterId: r.MatterId, Revision: revision + 1, State: "deleted"}
		if err = emit(ctx, tx, tenant, who, r.DocumentId, "document.deleted.v1", revision+1); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
