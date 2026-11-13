package workload

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/url"
	"testing"
	"time"
)

func TestIdentityRequiresBoundURIAndCN(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_TRUST_DOMAIN", "lawpilot.local")
	uri, _ := url.Parse("spiffe://lawpilot.local/api-gateway")
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "api-gateway"}}
	if Identity(cert, "api-gateway") {
		t.Fatal("CN-only accepted")
	}
	cert.URIs = []*url.URL{uri}
	if !Identity(cert, "api-gateway") {
		t.Fatal("valid identity rejected")
	}
	if Identity(cert, "execution-worker") {
		t.Fatal("wrong identity accepted")
	}
	uri.Host = "other.local"
	if Identity(cert, "api-gateway") {
		t.Fatal("foreign trust domain accepted")
	}
}
func TestProductionDatabaseTLS(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	for _, value := range []string{"postgres://db/x", "postgres://db/x?sslmode=require", "postgres://db/x?sslmode=disable"} {
		if DatabaseURL(value) == nil {
			t.Fatal("weak TLS accepted")
		}
	}
	if DatabaseURL("postgres://db/x?sslmode=verify-full") != nil {
		t.Fatal("verified TLS rejected")
	}
}
func TestGuardBoundsLifetimeAndConcurrency(t *testing.T) {
	guard := Guard(1, time.Second)
	started := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		guard(context.Background(), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("no deadline")
			}
			close(started)
			<-finish
			return nil, nil
		})
	}()
	<-started
	_, err := guard(context.Background(), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { t.Error("overload handler invoked"); return nil, nil })
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatal(err)
	}
	close(finish)
	<-done
}
