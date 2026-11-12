package contextengine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type principalKey struct{}

func actor(ctx context.Context) string { value, _ := ctx.Value(principalKey{}).(string); return value }
func Credentials(ca, certificate, key string) (credentials.TransportCredentials, error) {
	root, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(root) {
		return nil, errors.New("invalid CA")
	}
	pair, err := tls.LoadX509KeyPair(certificate, key)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{pair}}), nil
}
func Authenticator(ctx context.Context, issuer, jwks, audience, environment string) (grpc.UnaryServerInterceptor, error) {
	if issuer == "" || jwks == "" || audience == "" {
		return nil, errors.New("identity configuration required")
	}
	if environment != "development" && (!strings.HasPrefix(issuer, "https://") || !strings.HasPrefix(jwks, "https://")) {
		return nil, errors.New("production identity URLs require HTTPS")
	}
	verifier := oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, jwks), &oidc.Config{ClientID: audience, SupportedSigningAlgs: []string{"RS256"}})
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "WORKLOAD_IDENTITY_REQUIRED")
		}
		auth, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || len(auth.State.VerifiedChains) == 0 || len(auth.State.PeerCertificates) == 0 {
			return nil, status.Error(codes.Unauthenticated, "WORKLOAD_IDENTITY_REQUIRED")
		}
		identity := auth.State.PeerCertificates[0].Subject.CommonName
		if info.FullMethod == "/grpc.health.v1.Health/Check" {
			if identity != "api-gateway" && identity != "execution-worker" {
				return nil, status.Error(codes.PermissionDenied, "WORKLOAD_SCOPE_DENIED")
			}
			return next(ctx, req)
		}
		if info.FullMethod == "/lawpilot.context.v1.ContextService/VerifyExecutionEvidence" {
			if identity != "execution-worker" {
				return nil, status.Error(codes.PermissionDenied, "WORKLOAD_SCOPE_DENIED")
			}
			return next(ctx, req)
		}
		if identity != "api-gateway" {
			return nil, status.Error(codes.PermissionDenied, "WORKLOAD_SCOPE_DENIED")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		headers := md.Get("authorization")
		if len(headers) != 1 {
			return nil, status.Error(codes.Unauthenticated, "ACCESS_TOKEN_REQUIRED")
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, status.Error(codes.Unauthenticated, "ACCESS_TOKEN_REQUIRED")
		}
		token, err := verifier.Verify(ctx, parts[1])
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "INVALID_ACCESS_TOKEN")
		}
		var claims struct {
			Type string `json:"typ"`
		}
		if token.Claims(&claims) != nil || claims.Type != "Bearer" || strings.TrimSpace(token.Subject) == "" || token.IssuedAt.IsZero() || token.IssuedAt.After(time.Now().Add(time.Minute)) {
			return nil, status.Error(codes.Unauthenticated, "INVALID_ACCESS_TOKEN")
		}
		return next(context.WithValue(ctx, principalKey{}, token.Subject), req)
	}, nil
}
