package integrations

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type UploadSpec struct {
	URL     string
	Headers map[string]string
}
type ObjectStore interface {
	PresignPut(context.Context, string, string, int64, string) (UploadSpec, error)
	Get(context.Context, string, int64) ([]byte, error)
	Put(context.Context, string, string, []byte) error
	Delete(context.Context, string) error
	PresignGet(context.Context, string) (string, error)
}
type S3Store struct {
	Client                  *s3.Client
	Presigner               *s3.PresignClient
	Bucket, KMS, Encryption string
}

func endpoint(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && os.Getenv("APP_ENV") == "development")) {
		return errors.New("invalid outbound endpoint")
	}
	return nil
}
func NewStorage(ctx context.Context) (ObjectStore, error) {
	enabled, err := requiredFeature("FILE_STORAGE_ENABLED")
	if err != nil || !enabled {
		return nil, err
	}
	bucket, region := os.Getenv("S3_BUCKET"), os.Getenv("S3_REGION")
	kms := workload.Secret("S3_KMS_KEY_ID")
	if bucket == "" || region == "" || kms == "" {
		return nil, errors.New("S3 bucket, region and KMS key required")
	}
	options := []func(*config.LoadOptions) error{config.WithRegion(region), config.WithHTTPClient(&http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }})}
	if key := workload.Secret("S3_ACCESS_KEY_ID"); key != "" {
		secret := workload.Secret("S3_SECRET_ACCESS_KEY")
		if secret == "" {
			return nil, errors.New("S3 secret missing")
		}
		options = append(options, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(key, secret, workload.Secret("S3_SESSION_TOKEN"))))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, errors.New("S3 credentials invalid")
	}
	target := os.Getenv("S3_ENDPOINT")
	if target != "" {
		if err = endpoint(target); err != nil {
			return nil, err
		}
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if target != "" {
			o.BaseEndpoint = aws.String(target)
		}
		o.UsePathStyle = os.Getenv("S3_FORCE_PATH_STYLE") == "true"
		o.RetryMaxAttempts = 1
	})
	store := &S3Store{Client: client, Presigner: s3.NewPresignClient(client), Bucket: bucket, KMS: kms, Encryption: "aws:kms"}
	if os.Getenv("APP_ENV") != "development" || os.Getenv("S3_SKIP_BUCKET_POLICY_CHECK") != "true" {
		if err = store.Validate(ctx); err != nil {
			return nil, err
		}
	}
	return store, nil
}
func (s *S3Store) PresignPut(ctx context.Context, key, media string, size int64, hash string) (UploadSpec, error) {
	decoded, err := hex.DecodeString(hash)
	if err != nil {
		return UploadSpec{}, err
	}
	checksum := base64.StdEncoding.EncodeToString(decoded)
	r, err := s.Presigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: &s.Bucket, Key: &key, ContentType: &media, ContentLength: aws.Int64(size), ChecksumSHA256: &checksum, ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: &s.KMS}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return UploadSpec{}, errors.New("presign unavailable")
	}
	headers := map[string]string{}
	for name, values := range r.SignedHeader {
		headers[name] = strings.Join(values, ",")
	}
	headers["Content-Type"] = media
	return UploadSpec{r.URL, headers}, nil
}
func (s *S3Store) Get(ctx context.Context, key string, expected int64) ([]byte, error) {
	r, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return nil, errors.New("object read unavailable")
	}
	defer r.Body.Close()
	if r.ContentLength == nil || *r.ContentLength != expected || r.ServerSideEncryption != types.ServerSideEncryptionAwsKms || r.SSEKMSKeyId == nil || *r.SSEKMSKeyId != s.KMS {
		return nil, errors.New("object security metadata mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, expected+1))
	if err != nil || int64(len(data)) != expected {
		return nil, errors.New("object size mismatch")
	}
	return data, nil
}
func (s *S3Store) Put(ctx context.Context, key, media string, data []byte) error {
	sum, _ := hex.DecodeString(sha(data))
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.Bucket, Key: &key, ContentType: &media, ContentLength: aws.Int64(int64(len(data))), Body: bytes.NewReader(data), IfNoneMatch: aws.String("*"), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum)), ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: &s.KMS})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
			existing, readErr := s.Get(ctx, key, int64(len(data)))
			if readErr == nil && bytes.Equal(existing, data) {
				return nil
			}
			return errors.New("immutable object conflict")
		}
		return errors.New("object write unavailable")
	}
	return nil
}
func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.Bucket, Key: &key})
	if err != nil {
		return errors.New("object cleanup unavailable")
	}
	return nil
}
func (s *S3Store) PresignGet(ctx context.Context, key string) (string, error) {
	r, err := s.Presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: &key, ResponseContentDisposition: aws.String("attachment")}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		return "", errors.New("download unavailable")
	}
	return r.URL, nil
}

func (s *S3Store) Validate(ctx context.Context) error {
	public, err := s.Client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: &s.Bucket})
	if err != nil || public.PublicAccessBlockConfiguration == nil {
		return errors.New("bucket public-access protection required")
	}
	p := public.PublicAccessBlockConfiguration
	if !aws.ToBool(p.BlockPublicAcls) || !aws.ToBool(p.IgnorePublicAcls) || !aws.ToBool(p.BlockPublicPolicy) || !aws.ToBool(p.RestrictPublicBuckets) {
		return errors.New("bucket is not private")
	}
	encryption, err := s.Client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: &s.Bucket})
	if err != nil || encryption.ServerSideEncryptionConfiguration == nil {
		return errors.New("bucket KMS encryption required")
	}
	for _, rule := range encryption.ServerSideEncryptionConfiguration.Rules {
		if a := rule.ApplyServerSideEncryptionByDefault; a != nil && a.SSEAlgorithm == types.ServerSideEncryptionAwsKms && aws.ToString(a.KMSMasterKeyID) == s.KMS {
			return nil
		}
	}
	return errors.New("bucket KMS key mismatch")
}
