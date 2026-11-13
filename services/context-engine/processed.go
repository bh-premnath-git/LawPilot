package contextengine

import (
	"context"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
)

type processedKey struct{}
type fileProvenance struct {
	ID, SHA, Parser string
	Parts           []chunk
}

func (s *Service) IngestProcessed(ctx context.Context, r *pb.IngestProcessedRequest) (*pb.IngestTextResponse, error) {
	if !validID(r.FileId) || !validID(r.TenantId) || (r.ParserVersion != "isolated-text.v1" && r.ParserVersion != "isolated-ocr.v1") || len(r.Pages) == 0 || len(r.Pages) > 200 {
		return nil, bad("INVALID_PROCESSED_SOURCE")
	}
	var text strings.Builder
	parts := []chunk{}
	for i, p := range r.Pages {
		if p.Number != 0 && p.Number != int32(i+1) || p.Number == 0 && len(r.Pages) != 1 || strings.ContainsRune(p.Text, 0) {
			return nil, bad("INVALID_PAGE_MAPPING")
		}
		if i > 0 {
			text.WriteString("\n\f\n")
		}
		offset := text.Len()
		text.WriteString(p.Text)
		if text.Len() > 262144 {
			return nil, bad("EXTRACTED_TEXT_TOO_LARGE")
		}
		for _, c := range chunks(p.Text) {
			c.Start += offset
			c.End += offset
			if p.Number > 0 {
				n := p.Number
				c.Page = &n
			}
			parts = append(parts, c)
		}
	}
	who, matter, title, sourceSHA, parsedSHA, state, scan := "", "", "", "", "", "", ""
	err := s.tx(ctx, r.TenantId, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT principal_id,matter_id::text,filename,source_sha256,coalesce(parsed_sha256,''),state,scan_status FROM file_ingestions WHERE tenant_id=$1 AND id=$2", r.TenantId, r.FileId).Scan(&who, &matter, &title, &sourceSHA, &parsedSHA, &state, &scan)
	})
	if err != nil {
		return nil, err
	}
	if (state != "processing" && state != "completed") || scan != "clean" || digest(text.String()) != parsedSHA {
		return nil, status.Error(codes.FailedPrecondition, "FILE_NOT_VERIFIED")
	}
	trusted := context.WithValue(context.WithValue(ctx, principalKey{}, who), processedKey{}, fileProvenance{ID: r.FileId, SHA: sourceSHA, Parser: r.ParserVersion, Parts: parts})
	return s.IngestText(trusted, &pb.IngestTextRequest{Scope: &pb.Scope{TenantId: r.TenantId, RequestId: r.FileId}, MatterId: matter, Title: title, Text: text.String(), IdempotencyKey: "file:" + r.FileId})
}
