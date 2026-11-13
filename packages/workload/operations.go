package workload

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// StartOperations is optional and exposes only fixed, aggregate metrics. Bind it
// on an internal interface and enforce network policy before remote scraping.
func StartOperations(ctx context.Context, address string, ready func(context.Context) error, queues ...func(context.Context) (map[string]int64, error)) error {
	if address == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("up\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		check, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if ready != nil && ready(check) != nil {
			http.Error(w, "not ready", 503)
			return
		}
		w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "lawpilot_rpc_calls_total %d\nlawpilot_rpc_rejected_total %d\n", Calls.Load(), Rejected.Load())
		if len(queues) > 0 {
			call, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			metrics, err := queues[0](call)
			cancel()
			if err != nil {
				fmt.Fprintln(w, "lawpilot_queue_metrics_available 0")
			} else {
				fmt.Fprintln(w, "lawpilot_queue_metrics_available 1")
				for name, value := range metrics {
					fmt.Fprintf(w, "lawpilot_queue_records{state=%q} %d\n", name, value)
				}
			}
		}
		certPath := os.Getenv("GRPC_SERVER_CERT_FILE")
		if certPath == "" {
			certPath = os.Getenv("GRPC_CLIENT_CERT_FILE")
		}
		data, err := os.ReadFile(certPath)
		if err == nil {
			block, _ := pem.Decode(data)
			if block != nil {
				if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
					fmt.Fprintf(w, "lawpilot_certificate_expiry_seconds %d\n", int64(time.Until(cert.NotAfter).Seconds()))
				}
			}
		}
	})
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	go server.Serve(listener)
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(stop)
	}()
	return nil
}
