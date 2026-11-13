package calendar

import (
	"context"
	"github.com/bh-premnath-git/LawPilot/packages/workload"

	"errors"

	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"google.golang.org/grpc/status"
)

type principalKey struct{}

func actor(ctx context.Context) string { value, _ := ctx.Value(principalKey{}).(string); return value }
func Credentials(ca, certificate, key string) (credentials.TransportCredentials, error) {
	return workload.ServerCredentials(ca, certificate, key)
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
		identity := ""
		for _, role := range []string{"api-gateway", "execution-worker", "integration-service"} {
			if workload.Caller(ctx, role) {
				identity = role
				break
			}
		}
		if identity == "" {
			return nil, status.Error(codes.PermissionDenied, "WORKLOAD_SCOPE_DENIED")
		}

		if info.FullMethod == "/grpc.health.v1.Health/Check" {
			if identity != "api-gateway" && identity != "execution-worker" {
				return nil, status.Error(codes.PermissionDenied, "WORKLOAD_SCOPE_DENIED")
			}
			return next(ctx, req)
		}
		internal := strings.HasSuffix(info.FullMethod, "/LoadReminderWork") || strings.HasSuffix(info.FullMethod, "/DeliverReminder")
		if internal {
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
