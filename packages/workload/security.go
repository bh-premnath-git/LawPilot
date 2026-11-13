// Package workload owns local workload identities, rotating TLS and bounded RPCs.
package workload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

func Secret(name string) string {
	if file := os.Getenv(name + "_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	return os.Getenv(name)
}
func roots(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid trust bundle")
	}
	return pool, nil
}
func ServerCredentials(ca, cert, key string) (credentials.TransportCredentials, error) {
	makeConfig := func() (*tls.Config, error) {
		pool, err := roots(ca)
		if err != nil {
			return nil, err
		}
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{pair}}, nil
	}
	initial, err := makeConfig()
	if err != nil {
		return nil, err
	}
	initial.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) { return makeConfig() }
	return credentials.NewTLS(initial), nil
}
func ClientCredentials(server, identity string) (credentials.TransportCredentials, error) {
	ca, cert, key := os.Getenv("GRPC_CA_FILE"), os.Getenv("GRPC_CLIENT_CERT_FILE"), os.Getenv("GRPC_CLIENT_KEY_FILE")
	pool, err := roots(ca)
	if err != nil {
		return nil, err
	}
	load := func() (*tls.Certificate, error) {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || !Identity(leaf, identity) {
			return nil, errors.New("wrong workload identity")
		}
		return &pair, nil
	}
	if _, err = load(); err != nil {
		return nil, err
	}
	_ = pool
	return &rotatingClient{server, identity}, nil
}
func Identity(certificate *x509.Certificate, expected string) bool {
	if certificate.Subject.CommonName != expected {
		return false
	}
	domain := os.Getenv("WORKLOAD_TRUST_DOMAIN")
	if domain == "" {
		domain = "lawpilot.local"
	}
	for _, uri := range certificate.URIs {
		if uri.Scheme == "spiffe" && uri.Host == domain && uri.Path == "/"+expected && uri.RawQuery == "" && uri.Fragment == "" {
			return true
		}
	}
	return os.Getenv("APP_ENV") == "development" && os.Getenv("WORKLOAD_ALLOW_LEGACY_CN") == "true"
}
func Caller(ctx context.Context, expected string) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	auth, ok := p.AuthInfo.(credentials.TLSInfo)
	return ok && len(auth.State.VerifiedChains) > 0 && len(auth.State.PeerCertificates) > 0 && Identity(auth.State.PeerCertificates[0], expected)
}
func DatabaseURL(value string) error {
	if os.Getenv("APP_ENV") != "production" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Query().Get("sslmode") != "verify-full" {
		return errors.New("production database TLS verify-full required")
	}
	return nil
}

var Calls atomic.Uint64
var Rejected atomic.Uint64

// Bounds concurrency and lifetimes, including when an upstream omits a deadline.
func Guard(limit int, maximum time.Duration) grpc.UnaryServerInterceptor {
	slots := make(chan struct{}, limit)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		Calls.Add(1)
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			Rejected.Add(1)
			return nil, status.Error(codes.ResourceExhausted, "WORKLOAD_BUSY")
		}
		deadline, cancel := context.WithTimeout(ctx, maximum)
		defer cancel()
		return next(deadline, req)
	}
}

// Credentials are reread for every new transport handshake so a rolled trust
// bundle/leaf does not require retaining the previous certificate in memory.
type rotatingClient struct{ server, identity string }

func (r *rotatingClient) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	pool, err := roots(os.Getenv("GRPC_CA_FILE"))
	if err != nil {
		return nil, nil, err
	}
	pair, err := tls.LoadX509KeyPair(os.Getenv("GRPC_CLIENT_CERT_FILE"), os.Getenv("GRPC_CLIENT_KEY_FILE"))
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !Identity(leaf, r.identity) {
		return nil, nil, errors.New("wrong workload identity")
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: r.server, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || !Identity(state.PeerCertificates[0], r.server) {
			return errors.New("wrong server workload identity")
		}
		return nil
	}, Certificates: []tls.Certificate{pair}}).ClientHandshake(ctx, authority, raw)
}
func (r *rotatingClient) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("client credentials used as server")
}
func (r *rotatingClient) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.3", ServerName: r.server}
}
func (r *rotatingClient) Clone() credentials.TransportCredentials {
	return &rotatingClient{r.server, r.identity}
}
func (r *rotatingClient) OverrideServerName(name string) error {
	if name != r.server {
		return errors.New("workload server identity is pinned")
	}
	return nil
}
