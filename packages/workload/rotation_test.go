package workload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	pb "google.golang.org/grpc/health/grpc_health_v1"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotatingTLSNegotiatesHTTP2AndRejectsWrongURI(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_TRUST_DOMAIN", "lawpilot.local")
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	caFile := filepath.Join(dir, "ca.pem")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600)
	serial := int64(1)
	issue := func(name, uriRole string) (string, string) {
		t.Helper()
		serial++
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		uri, _ := url.Parse("spiffe://lawpilot.local/" + uriRole)
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, URIs: []*url.URL{uri}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		pk, _ := x509.MarshalECPrivateKey(key)
		certFile, keyFile := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
		os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk}), 0600)
		return certFile, keyFile
	}
	serverCert, serverKey := issue("context-engine", "context-engine")
	clientCert, clientKey := issue("api-gateway", "api-gateway")
	t.Setenv("GRPC_CA_FILE", caFile)
	t.Setenv("GRPC_CLIENT_CERT_FILE", clientCert)
	t.Setenv("GRPC_CLIENT_KEY_FILE", clientKey)
	creds, err := ServerCredentials(caFile, serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterHealthServer(server, health.NewServer())
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	go server.Serve(listener)
	defer server.Stop()
	clientCreds, err := ClientCredentials("context-engine", "api-gateway")
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(clientCreds))
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = pb.NewHealthClient(connection).Check(ctx, &pb.HealthCheckRequest{})
		if (err == nil) != want {
			t.Fatalf("TLS success=%v wanted=%v: %v", err == nil, want, err)
		}
	}
	check(true)
	issue("context-engine", "context-engine")
	issue("api-gateway", "api-gateway")
	check(true) // same credentials object reloads replacement leaves
	issue("context-engine", "other-service")
	check(false) // DNS/CN alone cannot replace SPIFFE scope
}
