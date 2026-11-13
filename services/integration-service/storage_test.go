package integrations

import (
	"bytes"
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestReleasedObjectsAreConditionalEncryptedAndIdempotent(t *testing.T) {
	var stored []byte
	puts := 0
	const kms = "arn:synthetic:kms"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts++
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("x-amz-server-side-encryption") != "aws:kms" || r.Header.Get("x-amz-server-side-encryption-aws-kms-key-id") != kms || r.Header.Get("x-amz-checksum-sha256") == "" {
				t.Error("object security headers missing")
			}
			if stored != nil {
				w.WriteHeader(412)
				io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
				return
			}
			stored, _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", "\"synthetic\"")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(stored)))
		w.Header().Set("x-amz-server-side-encryption", "aws:kms")
		w.Header().Set("x-amz-server-side-encryption-aws-kms-key-id", kms)
		w.Write(stored)
	}))
	defer server.Close()
	client := s3.NewFromConfig(aws.Config{Region: "test", Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("test", "test", "")), HTTPClient: server.Client()}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	store := &S3Store{Client: client, Bucket: "bucket", KMS: kms}
	ctx := context.Background()
	if err := store.Put(ctx, "released", "application/pdf", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "released", "application/pdf", []byte("original")); err != nil {
		t.Fatal("idempotent original rejected", err)
	}
	if err := store.Put(ctx, "released", "application/pdf", []byte("modified")); err == nil {
		t.Fatal("original overwritten")
	}
	if puts != 3 || !bytes.Equal(stored, []byte("original")) {
		t.Fatal("immutable object changed")
	}
}
