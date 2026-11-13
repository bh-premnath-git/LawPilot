package integrations

import (
	"context"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"os"
	"testing"
	"time"
)

func TestLiveReadOnlyProviderConformance(t *testing.T) {
	if os.Getenv("LIVE_PROVIDER_CONFORMANCE") != "true" {
		t.Skip("requires explicit operator opt-in and sandbox credentials")
	}
	providers, _, err := NewProviders()
	if err != nil {
		t.Fatal("provider configuration invalid")
	}
	token := workload.Secret("PROVIDER_SANDBOX_ACCESS_TOKEN")
	if token == "" || len(providers) == 0 {
		t.Fatal("sandbox token and enabled provider required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for name, p := range providers {
		for _, kind := range []string{"matter", "calendar", "calendar_entry"} {
			if _, err := p.Fetch(ctx, kind, "", token); err != nil {
				t.Fatalf("%s %s contract rejected; no payload or credentials logged", name, kind)
			}
		}
	}
}
