package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type parser struct {
	pb.UnimplementedDocumentParserServer
}

func (p *parser) Parse(ctx context.Context, r *pb.ParseRequest) (*pb.ParseResponse, error) {
	if !workload.Caller(ctx, "integration-service") {
		return nil, status.Error(codes.PermissionDenied, "WORKLOAD_SCOPE_DENIED")
	}
	sum := sha256.Sum256(r.Data)
	if len(r.Data) < 1 || len(r.Data) > 10*1024*1024 || hex.EncodeToString(sum[:]) != r.ExpectedSha256 || (r.MediaType != "application/pdf" && r.MediaType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document") {
		return nil, status.Error(codes.InvalidArgument, "INVALID_SOURCE")
	}
	call, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	binary := os.Getenv("DOCUMENT_PROCESSOR_BIN")
	if binary == "" {
		binary = "/usr/local/bin/lawpilot-document-worker"
	}
	command := exec.CommandContext(call, binary)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = time.Second
	raw, _ := json.Marshal(map[string]string{"operation": "parse.v1", "bytes_base64": base64.StdEncoding.EncodeToString(r.Data), "media_type": r.MediaType})
	command.Stdin = bytes.NewReader(raw)
	command.Stderr = io.Discard
	pipe, err := command.StdoutPipe()
	if err != nil {
		return nil, status.Error(codes.Unavailable, "PARSER_UNAVAILABLE")
	}
	if err = command.Start(); err != nil {
		return nil, status.Error(codes.Unavailable, "PARSER_UNAVAILABLE")
	}
	output, readErr := io.ReadAll(io.LimitReader(pipe, 2*1024*1024+1))
	if len(output) > 2*1024*1024 {
		_ = command.Cancel()
	}
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil || len(output) > 2*1024*1024 {
		return nil, status.Error(codes.InvalidArgument, "DOCUMENT_PROCESSING_REJECTED")
	}
	var result struct {
		Status string `json:"status"`
		SHA    string `json:"sha256"`
		Parser string `json:"parser_version"`
		Pages  []struct {
			Number int32  `json:"number"`
			Text   string `json:"text"`
		} `json:"pages"`
	}
	if json.Unmarshal(output, &result) != nil || result.SHA != r.ExpectedSha256 || result.Status != "ok" || result.Parser != "isolated-text.v1" || len(result.Pages) > 200 {
		return nil, status.Error(codes.InvalidArgument, "INVALID_PARSER_RESULT")
	}
	reply := &pb.ParseResponse{SourceSha256: result.SHA, ParserVersion: result.Parser}
	for _, page := range result.Pages {
		reply.Pages = append(reply.Pages, &pb.ParsedPage{Number: page.Number, Text: page.Text})
	}
	return reply, nil
}
func main() {
	creds, err := workload.ServerCredentials(os.Getenv("GRPC_CA_FILE"), os.Getenv("GRPC_SERVER_CERT_FILE"), os.Getenv("GRPC_SERVER_KEY_FILE"))
	if err != nil {
		log.Fatal("parser TLS required")
	}
	server := grpc.NewServer(grpc.Creds(creds), grpc.UnaryInterceptor(workload.Guard(4, 30*time.Second)), grpc.MaxRecvMsgSize(11*1024*1024), grpc.MaxSendMsgSize(2*1024*1024))
	pb.RegisterDocumentParserServer(server, &parser{})
	address := os.Getenv("PARSER_GRPC_ADDRESS")
	if address == "" {
		address = ":50056"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("parser listen failed")
	}
	log.Println("isolated document parser started")
	if server.Serve(listener) != nil {
		log.Fatal("parser stopped")
	}
}
