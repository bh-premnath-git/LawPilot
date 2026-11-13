package rpcclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"go.temporal.io/sdk/client"
	"os"
)

func TemporalOptions(address, namespace string) (client.Options, error) {
	options := client.Options{HostPort: address, Namespace: namespace}
	enabled := os.Getenv("TEMPORAL_TLS_ENABLED") == "true"
	if os.Getenv("APP_ENV") == "production" && !enabled {
		return options, errors.New("production Temporal TLS required")
	}
	if enabled {
		data, err := os.ReadFile(os.Getenv("TEMPORAL_CA_FILE"))
		if err != nil {
			return options, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return options, errors.New("invalid Temporal CA")
		}
		name := os.Getenv("TEMPORAL_SERVER_NAME")
		if name == "" {
			return options, errors.New("Temporal server name required")
		}
		secure := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: name}
		if cert := os.Getenv("TEMPORAL_CLIENT_CERT_FILE"); cert != "" {
			key, err := tls.LoadX509KeyPair(cert, os.Getenv("TEMPORAL_CLIENT_KEY_FILE"))
			if err != nil {
				return options, err
			}
			secure.Certificates = []tls.Certificate{key}
		}
		options.ConnectionOptions.TLS = secure
	}
	if apiKey := workload.Secret("TEMPORAL_API_KEY"); apiKey != "" {
		if !enabled {
			return options, errors.New("Temporal API key requires TLS")
		}
		options.Credentials = client.NewAPIKeyStaticCredentials(apiKey)
	}
	return options, nil
}
