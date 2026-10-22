package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Close is documented as safe to call more than once, and ListenAndServe
// calls it on shutdown, so a kit that also defers Close calls it twice. With a
// minted workload token behind it, the second call used to panic ("close of
// closed channel") in the token manager.
func TestRuntime_CloseTwiceWithTokenManager(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted", "expires_at": time.Now().Add(time.Hour)})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PLATFORM_GATEWAY_URL", srv.URL)
	t.Setenv("OGA_TENANT_ID", "tnt1")
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "s")
	t.Setenv("AGENT_REGISTRATION_ID", "tnt1.tools-mcp")

	rt, err := NewRuntimeFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewRuntimeFromEnv: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
