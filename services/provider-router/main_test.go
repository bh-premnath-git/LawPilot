package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSignature(t *testing.T) {
	secret := "unit-test-secret-value-at-least-32-characters"
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	body := []byte(`{"tenantId":"test"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !authorized(body, ts, sig, secret, time.Now()) {
		t.Fatal("valid signature was denied")
	}
	if authorized([]byte(`{"tenantId":"other"}`), ts, sig, secret, time.Now()) {
		t.Fatal("forged body was authorized")
	}
}
func TestManifestDoesNotClaimClioSupport(t *testing.T) {
	c, ok := manifest("clio-manage")
	if !ok {
		t.Fatal("missing clio manifest")
	}
	if len(c.Capabilities) != 0 {
		t.Fatal("unimplemented Clio adapter advertises live capabilities")
	}
}

func TestInvokeRejectsTrailingJSONAndInvalidTenant(t *testing.T) {
	secret := "unit-test-secret-value-at-least-32-characters"
	valid := `{"tenantId":"11111111-1111-4111-8111-111111111111","principalId":"user","providerId":"mock","capabilityId":"matter.read.v1","resourceId":"22222222-2222-4222-8222-222222222222","operationId":"read-1"}`
	for _, test := range []struct {
		body   string
		status int
	}{{valid, 200}, {valid + ` {"extra":true}`, 400}, {strings.Replace(valid, "11111111-1111-4111-8111-111111111111", "bad", 1), 400}, {strings.Replace(valid, "matter.read.v1", "arbitrary.write.v1", 1), 422}} {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "." + test.body))
		req := httptest.NewRequest("POST", "/internal/invoke", strings.NewReader(test.body))
		req.Header.Set("X-Lawpilot-Timestamp", ts)
		req.Header.Set("X-Lawpilot-Signature", hex.EncodeToString(mac.Sum(nil)))
		res := httptest.NewRecorder()
		newHandler(secret).ServeHTTP(res, req)
		if res.Code != test.status {
			t.Fatalf("got %d want %d: %s", res.Code, test.status, res.Body.String())
		}
	}
}
