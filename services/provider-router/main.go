package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Capability struct {
	ID              string `json:"id"`
	ContractVersion string `json:"contractVersion"`
	Supported       bool   `json:"supported"`
}
type Manifest struct {
	ProviderID     string       `json:"providerId"`
	AdapterVersion string       `json:"adapterVersion"`
	Capabilities   []Capability `json:"capabilities"`
}
type Request struct {
	TenantID     string `json:"tenantId"`
	PrincipalID  string `json:"principalId"`
	ProviderID   string `json:"providerId"`
	CapabilityID string `json:"capabilityId"`
	ResourceID   string `json:"resourceId"`
	OperationID  string `json:"operationId"`
}
type Response struct {
	Status   string `json:"status"`
	Resource any    `json:"resource,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Local transport authentication only. Production: workload identity with mTLS.
func authorized(body []byte, ts, sig, secret string, now time.Time) bool {
	if len(secret) < 32 || len(ts) != 13 || len(sig) != 64 {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || abs(now.UnixMilli()-n) > 60_000 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	given, err := hex.DecodeString(sig)
	return err == nil && hmac.Equal(mac.Sum(nil), given)
}
func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
func manifest(id string) (Manifest, bool) {
	switch id {
	case "mock":
		return Manifest{"mock", "0.1.0", []Capability{{"matter.read.v1", "1.0", true}}}, true
	case "clio-manage":
		return Manifest{"clio-manage", "0.1.0", []Capability{}}, true // No live API operations until OAuth adapter is implemented.
	default:
		return Manifest{}, false
	}
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type Matter struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Title    string `json:"title"`
	Status   string `json:"status"`
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func newHandler(secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]string{"service": "provider-router", "status": "up", "liveClio": "not_implemented"})
	})
	mux.HandleFunc("/internal/manifest/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			jsonOut(w, 405, Response{Error: "method_not_allowed"})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/internal/manifest/")
		// INTERNAL endpoint: require signed empty payload.
		if !authorized(nil, r.Header.Get("X-Lawpilot-Timestamp"), r.Header.Get("X-Lawpilot-Signature"), secret, time.Now()) {
			jsonOut(w, 401, Response{Error: "unauthorized"})
			return
		}
		m, ok := manifest(id)
		if !ok {
			jsonOut(w, 404, Response{Error: "provider_not_found"})
			return
		}
		jsonOut(w, 200, m)
	})
	mux.HandleFunc("/internal/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			jsonOut(w, 405, Response{Error: "method_not_allowed"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			jsonOut(w, 413, Response{Error: "request_too_large"})
			return
		}
		if !authorized(raw, r.Header.Get("X-Lawpilot-Timestamp"), r.Header.Get("X-Lawpilot-Signature"), secret, time.Now()) {
			jsonOut(w, 401, Response{Error: "unauthorized"})
			return
		}
		var in Request
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || !uuidPattern.MatchString(in.TenantID) || strings.TrimSpace(in.PrincipalID) == "" || strings.TrimSpace(in.OperationID) == "" || !uuidPattern.MatchString(in.ResourceID) {
			jsonOut(w, 400, Response{Error: "invalid_request"})
			return
		}
		if dec.Decode(&struct{}{}) != io.EOF {
			jsonOut(w, 400, Response{Error: "invalid_request"})
			return
		}
		m, ok := manifest(in.ProviderID)
		if !ok {
			jsonOut(w, 422, Response{Error: "provider_unknown"})
			return
		}
		supported := false
		for _, c := range m.Capabilities {
			if c.ID == in.CapabilityID && c.Supported {
				supported = true
			}
		}
		if !supported {
			jsonOut(w, 422, Response{Error: "UNSUPPORTED_CAPABILITY"})
			return
		}
		// IMPORTANT: mock data only. In production resolve provider from tenant/resource ownership DB, not model-supplied hint.
		if in.ProviderID == "mock" && in.CapabilityID == "matter.read.v1" {
			jsonOut(w, 200, Response{Status: "ok", Resource: Matter{ID: in.ResourceID, TenantID: in.TenantID, Title: "Synthetic test matter", Status: "mock"}})
			return
		}
		jsonOut(w, 501, Response{Error: "NOT_IMPLEMENTED"})
	})
	return mux
}
func main() {
	secret := os.Getenv("INTERNAL_HMAC_KEY")
	if len(secret) < 32 {
		log.Fatal("INTERNAL_HMAC_KEY must be >= 32 characters")
	}
	addr := ":8082"
	fmt.Println("provider-router listening on", addr)
	server := &http.Server{Addr: addr, Handler: newHandler(secret), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	log.Fatal(server.ListenAndServe())
}
