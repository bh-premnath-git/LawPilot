package integrations

import (
	"context"
	"errors"
	ctxpb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"regexp"
	"sort"
	"strings"
	"time"
)

const PDF = "application/pdf"
const DOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

type fileRecord struct {
	ID, Matter, Principal, Name, Media, SHA, Quarantine, Clean, State, Scan, Document, Version, Error string
	Size                                                                                              int64
	Expires                                                                                           time.Time
}

func loadFile(ctx context.Context, tx pgx.Tx, tenant, id string) (fileRecord, error) {
	var f fileRecord
	err := tx.QueryRow(ctx, "SELECT id::text,matter_id::text,principal_id,filename,media_type,source_sha256,quarantine_key,clean_key,state,scan_status,coalesce(document_id::text,''),coalesce(version_id::text,''),coalesce(error_code,''),size_bytes,expires_at FROM file_ingestions WHERE tenant_id=$1 AND id=$2", tenant, id).Scan(&f.ID, &f.Matter, &f.Principal, &f.Name, &f.Media, &f.SHA, &f.Quarantine, &f.Clean, &f.State, &f.Scan, &f.Document, &f.Version, &f.Error, &f.Size, &f.Expires)
	return f, err
}
func (f fileRecord) public() *pb.File {
	return &pb.File{Id: f.ID, MatterId: f.Matter, State: f.State, ErrorCode: f.Error, DocumentId: f.Document, VersionId: f.Version}
}
func (s *Service) CreateUpload(ctx context.Context, r *pb.CreateUploadRequest) (*pb.CreateUploadResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if s.Storage == nil {
		return nil, disabled()
	}
	if !validID(r.MatterId) || r.SizeBytes < 1 || r.SizeBytes > 10*1024*1024 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(r.Sha256) || len(strings.TrimSpace(r.Filename)) < 1 || len(r.Filename) > 200 || (r.MediaType != PDF && r.MediaType != DOCX) {
		return nil, bad("INVALID_FILE_UPLOAD")
	}
	who := actor(ctx)
	reply := new(pb.CreateUploadResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, who, "document.ingest", r.MatterId); err != nil {
			return err
		}
		if err := lock(ctx, tx, tenant, r.MatterId); err != nil {
			return err
		}
		hash := fingerprint("file-upload", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		id := uuid.NewString()
		quarantine := "quarantine/" + tenant + "/" + id
		clean := "documents/" + tenant + "/" + id + "/original"
		if _, err = tx.Exec(ctx, "INSERT INTO file_ingestions(id,tenant_id,matter_id,principal_id,filename,media_type,size_bytes,source_sha256,quarantine_key,clean_key,expires_at)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", id, tenant, r.MatterId, who, r.Filename, r.MediaType, r.SizeBytes, r.Sha256, quarantine, clean, time.Now().Add(15*time.Minute)); err != nil {
			return err
		}
		reply.File = &pb.File{Id: id, MatterId: r.MatterId, State: "awaiting_upload"}
		if err = audit(ctx, tx, tenant, who, id, "file.upload.created", "quarantine"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	if err != nil {
		return nil, err
	}
	var f fileRecord
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		f, err = loadFile(ctx, tx, tenant, reply.File.Id)
		if err != nil {
			return err
		}
		return access(ctx, tx, tenant, who, "document.ingest", f.Matter)
	})
	if err != nil {
		return nil, err
	}
	reply.File = f.public()
	if f.State != "awaiting_upload" {
		return reply, nil
	}
	if !f.Expires.After(time.Now()) {
		return nil, status.Error(codes.FailedPrecondition, "UPLOAD_EXPIRED")
	}
	spec, err := s.Storage.PresignPut(ctx, f.Quarantine, f.Media, f.Size, f.SHA)
	if err != nil {
		return nil, unavailable("STORAGE_UNAVAILABLE")
	}
	reply.UploadUrl = spec.URL
	reply.ExpiresSeconds = int32(time.Until(f.Expires).Seconds())
	names := []string{}
	for name := range spec.Headers {
		if !strings.EqualFold(name, "Host") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		reply.RequiredHeaders = append(reply.RequiredHeaders, &pb.Header{Name: name, Value: spec.Headers[name]})
	}
	return reply, nil
}
func (s *Service) FinalizeUpload(ctx context.Context, r *pb.FileRequest) (*pb.FileResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if s.Storage == nil {
		return nil, disabled()
	}
	if !validID(r.FileId) || !validID(r.MatterId) {
		return nil, bad("INVALID_FILE")
	}
	reply := new(pb.FileResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, tenant, r.FileId); err != nil {
			return err
		}
		f, err := loadFile(ctx, tx, tenant, r.FileId)
		if err != nil {
			return err
		}
		if f.Principal != actor(ctx) || !strings.EqualFold(f.Matter, r.MatterId) {
			return denied()
		}
		if err = access(ctx, tx, tenant, actor(ctx), "document.ingest", f.Matter); err != nil {
			return err
		}
		if f.State == "awaiting_upload" {
			if !f.Expires.After(time.Now()) {
				return status.Error(codes.FailedPrecondition, "UPLOAD_EXPIRED")
			}
			if _, err = tx.Exec(ctx, "UPDATE file_ingestions SET state='queued' WHERE tenant_id=$1 AND id=$2", tenant, f.ID); err != nil {
				return err
			}
			if err = job(ctx, tx, tenant, f.Principal, f.ID, "file", f.ID); err != nil {
				return err
			}
			if err = audit(ctx, tx, tenant, f.Principal, f.ID, "file.processing.queued", "durable"); err != nil {
				return err
			}
			f.State = "queued"
		}
		reply.File = f.public()
		return nil
	})
	return reply, err
}
func (s *Service) GetFile(ctx context.Context, r *pb.FileRequest) (*pb.FileResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.FileResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		f, err := loadFile(ctx, tx, tenant, r.FileId)
		if err != nil {
			return err
		}
		if f.Principal != actor(ctx) || !strings.EqualFold(f.Matter, r.MatterId) {
			return denied()
		}
		if err = access(ctx, tx, tenant, actor(ctx), "document.ingest", f.Matter); err != nil {
			return err
		}
		reply.File = f.public()
		return nil
	})
	return reply, err
}
func (s *Service) DownloadFile(ctx context.Context, r *pb.FileRequest) (*pb.DownloadResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if s.Storage == nil {
		return nil, disabled()
	}
	var key string
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		f, err := loadFile(ctx, tx, tenant, r.FileId)
		if err != nil {
			return err
		}
		if !strings.EqualFold(f.Matter, r.MatterId) {
			return denied()
		}
		if err = access(ctx, tx, tenant, actor(ctx), "document.download", f.Matter); err != nil {
			return err
		}
		if f.State != "completed" {
			return status.Error(codes.FailedPrecondition, "FILE_NOT_RELEASED")
		}
		var current bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM document_assets WHERE tenant_id=$1 AND id=$2 AND current_version_id=$3 AND deleted_at IS NULL)", tenant, f.Document, f.Version).Scan(&current); err != nil {
			return err
		}
		if !current {
			return status.Error(codes.FailedPrecondition, "SOURCE_NOT_CURRENT")
		}
		key = f.Clean
		return audit(ctx, tx, tenant, actor(ctx), f.ID, "file.download.authorized", "short-lived")
	})
	if err != nil {
		return nil, err
	}
	target, err := s.Storage.PresignGet(ctx, key)
	if err != nil {
		return nil, unavailable("STORAGE_UNAVAILABLE")
	}
	return &pb.DownloadResponse{DownloadUrl: target, ExpiresSeconds: 60}, nil
}
func (s *Service) rejectFile(ctx context.Context, tenant string, f fileRecord, code string, infected bool) (*pb.JobResponse, error) {
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		scan := f.Scan
		if infected {
			scan = "infected"
		}
		if _, err := tx.Exec(ctx, "UPDATE file_ingestions SET state='rejected',scan_status=$3,error_code=$4 WHERE tenant_id=$1 AND id=$2", tenant, f.ID, scan, code); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE integration_jobs SET state='rejected',error_code=$3,updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, f.ID, code); err != nil {
			return err
		}
		return audit(ctx, tx, tenant, f.Principal, f.ID, "file.processing.rejected", code)
	})
	if err == nil {
		_ = s.Storage.Delete(ctx, f.Quarantine)
	}
	return &pb.JobResponse{Id: f.ID, State: "rejected", ErrorCode: code}, err
}
func (s *Service) processFile(ctx context.Context, tenant, id string) (*pb.JobResponse, error) {
	if s.Storage == nil || s.Scanner == nil || s.Parser == nil || s.Context == nil {
		return nil, disabled()
	}
	var f fileRecord
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		f, err = loadFile(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if err = access(ctx, tx, tenant, f.Principal, "document.ingest", f.Matter); err != nil {
			return err
		}
		if f.State == "completed" || f.State == "rejected" {
			return nil
		}
		if f.State != "queued" && f.State != "processing" {
			return status.Error(codes.FailedPrecondition, "FILE_NOT_FINALIZED")
		}
		_, err = tx.Exec(ctx, "UPDATE file_ingestions SET state='processing' WHERE tenant_id=$1 AND id=$2", tenant, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if f.State == "completed" || f.State == "rejected" {
		return &pb.JobResponse{Id: id, State: f.State, ErrorCode: f.Error}, nil
	}
	data, err := s.Storage.Get(ctx, f.Quarantine, f.Size)
	if err != nil {
		return nil, unavailable("QUARANTINE_READ_UNAVAILABLE")
	}
	if sha(data) != f.SHA {
		return s.rejectFile(ctx, tenant, f, "FILE_CHECKSUM_MISMATCH", false)
	}
	clean, err := s.Scanner.Scan(ctx, data)
	if err != nil {
		return nil, unavailable("SCANNER_UNAVAILABLE")
	}
	if !clean {
		return s.rejectFile(ctx, tenant, f, "MALWARE_DETECTED", true)
	}
	parsed, err := s.Parser.Parse(ctx, &pb.ParseRequest{Data: data, MediaType: f.Media, ExpectedSha256: f.SHA})
	if err != nil {
		if status.Code(err) == codes.InvalidArgument {
			return s.rejectFile(ctx, tenant, f, "UNSUPPORTED_OR_MALFORMED_DOCUMENT", false)
		}
		return nil, unavailable("PARSER_UNAVAILABLE")
	}
	if parsed.SourceSha256 != f.SHA || (parsed.ParserVersion != "isolated-text.v1" && parsed.ParserVersion != "isolated-ocr.v1") || len(parsed.Pages) == 0 || len(parsed.Pages) > 200 {
		return s.rejectFile(ctx, tenant, f, "PARSER_INTEGRITY_MISMATCH", false)
	}
	text, err := JoinPages(parsed.Pages)
	if err != nil {
		return s.rejectFile(ctx, tenant, f, "EXTRACTED_TEXT_LIMIT_OR_MAPPING", false)
	}
	if err = s.Storage.Put(ctx, f.Clean, f.Media, data); err != nil {
		return nil, unavailable("STORAGE_UNAVAILABLE")
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, f.Principal, "document.ingest", f.Matter); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE file_ingestions SET scan_status='clean',parsed_sha256=$3 WHERE tenant_id=$1 AND id=$2 AND state='processing'", tenant, id, sha([]byte(text)))
		return err
	})
	if err != nil {
		return nil, err
	}
	pages := make([]*ctxpb.ProcessedPage, len(parsed.Pages))
	for i, p := range parsed.Pages {
		pages[i] = &ctxpb.ProcessedPage{Number: p.Number, Text: p.Text}
	}
	_, err = s.Context.IngestProcessed(ctx, &ctxpb.IngestProcessedRequest{TenantId: tenant, FileId: id, Pages: pages, ParserVersion: parsed.ParserVersion})
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE integration_jobs SET state='completed',error_code=NULL,updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, id)
		return err
	})
	if err == nil {
		_ = s.Storage.Delete(ctx, f.Quarantine)
	}
	return &pb.JobResponse{Id: id, State: "completed"}, err
}
func JoinPages(pages []*pb.ParsedPage) (string, error) {
	if len(pages) == 0 || len(pages) > 200 {
		return "", errors.New("page bounds")
	}
	var text strings.Builder
	for i, p := range pages {
		if p.Number != 0 && p.Number != int32(i+1) || p.Number == 0 && len(pages) != 1 || strings.ContainsRune(p.Text, 0) {
			return "", errors.New("page mapping")
		}
		if i > 0 {
			text.WriteString("\n\f\n")
		}
		text.WriteString(p.Text)
		if text.Len() > 262144 {
			return "", errors.New("text limit")
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("no text")
	}
	return text.String(), nil
}
