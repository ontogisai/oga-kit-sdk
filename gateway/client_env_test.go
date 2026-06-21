package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewClientFromEnv_BootstrapMintsAndAuthenticates verifies that in
// bootstrap mode the env client mints a workload token and sends it as the
// bearer on outbound tool calls (OGA-421 — the fix for the OGA-417 prereq-2
// gap where the tools sidecar called the gateway tokenless).
func TestNewClientFromEnv_BootstrapMintsAndAuthenticates(t *testing.T) {
	var lastAuth atomic.Value
	lastAuth.Store("")
	var issued atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token/issue":
			issued.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "minted-workload-token",
				"expires_at": time.Now().Add(time.Hour),
			})
		case "/mcp/tools/call":
			lastAuth.Store(r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PLATFORM_GATEWAY_URL", srv.URL)
	t.Setenv("OGA_TENANT_ID", "sgac1")
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "boot-s3cret")
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.built-env-tools-mcp")

	client, closer, err := NewClientFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	defer func() { _ = closer.Close() }()

	if issued.Load() == 0 {
		t.Error("expected a mint call to /auth/token/issue")
	}

	if _, err := client.CallTool(context.Background(), "kg_get_entity", map[string]any{"entity_id": "b1"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := lastAuth.Load().(string); got != "Bearer minted-workload-token" {
		t.Errorf("outbound Authorization = %q, want %q", got, "Bearer minted-workload-token")
	}
}

// TestNewClientFromEnv_TokenlessWhenNoBootstrap verifies the dev/tokenless path:
// no bootstrap env and no token file → no managed token, a no-op Closer, and no
// Authorization header on requests.
func TestNewClientFromEnv_TokenlessWhenNoBootstrap(t *testing.T) {
	var hadAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadAuth.Store(r.Header.Get("Authorization") != "")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	t.Setenv("PLATFORM_GATEWAY_URL", srv.URL)
	t.Setenv("OGA_TENANT_ID", "sgac1")
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")
	t.Setenv("AGENT_SERVICE_TOKEN_PATH", "")

	client, closer, err := NewClientFromEnv(context.Background())
	if err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	if closer == nil {
		t.Fatal("Closer must never be nil")
	}
	defer func() { _ = closer.Close() }() // no-op closer must not panic

	if _, err := client.CallTool(context.Background(), "kg_get_entity", map[string]any{}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if hadAuth.Load() {
		t.Error("expected no Authorization header in tokenless mode")
	}
}
