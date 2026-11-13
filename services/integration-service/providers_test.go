package integrations

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCredentialBindingAndRotation(t *testing.T) {
	k := &Keyring{Active: "one", Keys: map[string]string{"one": base64.StdEncoding.EncodeToString(make([]byte, 32)), "two": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))}}
	encrypted, err := k.Seal([]byte("synthetic-refresh"), "tenant|connection|clio")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = k.Open(encrypted, "other|connection|clio"); err == nil {
		t.Fatal("cross tenant credential accepted")
	}
	k.Active = "two"
	plain, err := k.Open(encrypted, "tenant|connection|clio")
	if err != nil || string(plain) != "synthetic-refresh" {
		t.Fatal("rotation lost old key")
	}
	encrypted[len(encrypted)/2] ^= 1
	if _, err = k.Open(encrypted, "tenant|connection|clio"); err == nil {
		t.Fatal("tamper accepted")
	}
}
func TestProviderContractPaginationRefreshAndConditionalWrite(t *testing.T) {
	for _, id := range []string{"clio-manage", "generic-rest"} {
		t.Run(id, func(t *testing.T) {
			var patch int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					r.ParseForm()
					if r.Form.Get("refresh_token") != "old" || r.Form.Get("client_secret") != "secret" {
						t.Error("refresh contract")
					}
					io.WriteString(w, `{"access_token":"new","expires_in":3600,"token_type":"bearer"}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer new" {
					t.Error("missing bearer")
				}
				if r.Method == "PATCH" {
					patch++
					if r.Header.Get("If-Match") != "v1" {
						t.Error("missing precondition")
					}
					w.WriteHeader(412)
					return
				}
				if r.URL.Query().Get("limit") != "100" {
					t.Error("missing bounded page")
				}
				io.WriteString(w, `{"data":[{"id":12,"etag":"v1","description":"Lease","title":"Lease","status":"active"}],"meta":{"paging":{}}}`)
			}))
			defer server.Close()
			p := &Provider{ID: id, Base: server.URL, TokenURL: server.URL + "/token", ClientID: "client", ClientSecret: "secret", HTTP: server.Client(), TimeZone: "UTC"}
			token, err := p.Exchange(context.Background(), "", "old")
			if err != nil || token.Refresh != "old" {
				t.Fatal("refresh rotation", err)
			}
			page, err := p.Fetch(context.Background(), "matter", "", token.Access)
			if err != nil || len(page.Matters) != 1 || page.Matters[0].ID != "12" {
				t.Fatal("mapping", err)
			}
			if _, err = p.safeURL("matter", "https://attacker.invalid/matters"); err == nil {
				t.Fatal("pagination exfiltration accepted")
			}
			if _, err = p.safeURL("matter", server.URL+"/other"); err == nil {
				t.Fatal("wrong resource accepted")
			}
			state, err := p.MatterUpdate(context.Background(), "12", "New", "v1", "new")
			if err != nil || state != "conflict" || patch != 1 {
				t.Fatal("conditional write", state, err)
			}
		})
	}
}
func TestPageProvenanceBounds(t *testing.T) {
	for _, pages := range [][]*pb.ParsedPage{{{Number: 2, Text: "bad"}}, {{Number: 0, Text: "a"}, {Number: 0, Text: "b"}}, {{Number: 1, Text: strings.Repeat("x", 262145)}}, {{Number: 1, Text: "\x00"}}} {
		if _, err := JoinPages(pages); err == nil {
			t.Fatal("bad page accepted")
		}
	}
	text, err := JoinPages([]*pb.ParsedPage{{Number: 1, Text: "first"}, {Number: 2, Text: "second"}})
	if err != nil || text != "first\n\f\nsecond" {
		t.Fatal(text, err)
	}
}
func TestClamStreamProtocolAndFailClosed(t *testing.T) {
	for _, reply := range []string{"stream: OK\x00", "stream: Eicar FOUND\x00", "stream: ERROR\x00"} {
		t.Run(strings.TrimSuffix(reply, "\x00"), func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			done := make(chan []byte, 1)
			go func() {
				c, e := l.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				prefix := make([]byte, 10)
				io.ReadFull(c, prefix)
				var data []byte
				for {
					var b [4]byte
					if _, e = io.ReadFull(c, b[:]); e != nil {
						return
					}
					n := binary.BigEndian.Uint32(b[:])
					if n == 0 {
						break
					}
					chunk := make([]byte, n)
					io.ReadFull(c, chunk)
					data = append(data, chunk...)
				}
				done <- append(prefix, data...)
				io.WriteString(c, reply)
			}()
			clean, err := (&ClamScanner{Address: l.Addr().String()}).Scan(context.Background(), []byte("synthetic"))
			if (reply == "stream: OK\x00") != clean {
				t.Fatal("scanner verdict")
			}
			if strings.Contains(reply, "ERROR") && err == nil {
				t.Fatal("scanner failed open")
			}
			if string(<-done) != "zINSTREAM\x00synthetic" {
				t.Fatal("stream framing")
			}
		})
	}
}
func TestDisabledConfigurationHasNoCredentialRequirement(t *testing.T) {
	for _, key := range []string{"CLIO_ENABLED", "GENERIC_PROVIDER_ENABLED", "FILE_STORAGE_ENABLED"} {
		t.Setenv(key, "false")
	}
	p, k, err := NewProviders()
	if err != nil || len(p) != 0 || k != nil {
		t.Fatal("disabled config", err)
	}
	store, err := NewStorage(context.Background())
	if err != nil || store != nil {
		t.Fatal("disabled storage", err)
	}
	t.Setenv("CLIO_ENABLED", "true")
	t.Setenv("CLIO_CLIENT_ID", "")
	if _, _, err = NewProviders(); err == nil {
		t.Fatal("enabled missing config accepted")
	}
}

func TestClioExpandedRecurrenceMetadataAndSparseFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("fields"), "recurrence_rule") {
			t.Error("recurrence metadata omitted")
		}
		io.WriteString(w, `{"data":[{"id":"12_20261201","etag":"e1","summary":"Recurring appointment","description":"Synthetic","start_at":"2026-12-01T10:00:00Z","end_at":"2026-12-01T11:00:00Z","all_day":false,"start_at_time_zone":"Europe/London","calendar_owner":{"id":2,"type":"UserCalendar"},"matter":{"id":1},"recurrence_rule":"FREQ=WEEKLY;COUNT=4","court_rule":false}]}`)
	}))
	defer server.Close()
	p := &Provider{ID: "clio-manage", Base: server.URL, HTTP: server.Client(), TimeZone: "UTC"}
	page, err := p.Fetch(context.Background(), "calendar_entry", "", "synthetic")
	if err != nil || len(page.Entries) != 1 {
		t.Fatal(page, err)
	}
	entry := page.Entries[0]
	if entry.Recurrence != "FREQ=WEEKLY;COUNT=4" || entry.Zone != "Europe/London" || entry.Calendar != "UserCalendar:2" {
		t.Fatal("canonical recurrence mapping lost", entry)
	}
}

func TestMissingProbeNeverTreatsAuthorizationOrNetworkFailureAsDeletion(t *testing.T) {
	for _, code := range []int{200, 403, 404, 410, 429, 500} {
		t.Run(string(rune(code)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
			defer server.Close()
			p := &Provider{ID: "generic-rest", Base: server.URL, HTTP: server.Client()}
			exists, err := p.Probe(context.Background(), "matter", "1", "synthetic")
			if code == 200 {
				if !exists || err != nil {
					t.Fatal(exists, err)
				}
			} else if code == 404 || code == 410 {
				if exists || err != nil {
					t.Fatal(exists, err)
				}
			} else if err == nil {
				t.Fatal("error response became deletion")
			}
		})
	}
}

func TestExtensionFlagsFailClosedWhenConfigurationIsMissing(t *testing.T) {
	t.Setenv("PROVIDER_WEBHOOKS_ENABLED", "true")
	t.Setenv("PROVIDER_WEBHOOK_PUBLIC_URL", "")
	t.Setenv("PROVIDER_CALENDAR_WRITES_ENABLED", "false")
	if ValidateExtensions(map[string]*Provider{}, false) == nil {
		t.Fatal("unconfigured webhooks accepted")
	}
	t.Setenv("PROVIDER_WEBHOOKS_ENABLED", "false")
	t.Setenv("PROVIDER_CALENDAR_WRITES_ENABLED", "true")
	if ValidateExtensions(map[string]*Provider{}, false) == nil {
		t.Fatal("calendar writes enabled without provider policy")
	}
}
