package rpcclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"google.golang.org/grpc/credentials"
	"os"
)

func Credentials(server string) (credentials.TransportCredentials, error) {
	root, err := os.ReadFile(os.Getenv("GRPC_CA_FILE"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(root) {
		return nil, errors.New("invalid CA")
	}
	certificate, err := tls.LoadX509KeyPair(os.Getenv("GRPC_CLIENT_CERT_FILE"), os.Getenv("GRPC_CLIENT_KEY_FILE"))
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.Subject.CommonName != "execution-worker" {
		return nil, errors.New("execution-worker identity required")
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate}, ServerName: server}), nil
}
