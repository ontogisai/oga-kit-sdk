package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// issueGateway is an httptest gateway serving POST /auth/token/issue and
// /mcp/tools/call. It counts mints and records the Authorization header of the
// last tool call.
func issueGateway(t *testing.T) (url string, issueHits *atomic.Int32, lastToolAuth *atomic.Value) {
	t.Helper()
	var iss atomic.Int32
	var auth atomic.Value
	auth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/token/issue":
			iss.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "minted-workload-jwt",
				"expires_at": time.Now().Add(12 * time.Hour).UTC(),
			})
		case "/mcp/tools/call":
			auth.Store(r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &iss, &auth
}

// TestConnectRuntimeDeps_BootstrapMintMode verifies the chassis wiring: with
// the manager-injected bootstrap identity, the chassis mints the workload token
// from /auth/token/issue and sends it on gateway calls.
func TestConnectRuntimeDeps_BootstrapMintMode(t *testing.T) {
	url, issueHits, lastAuth := issueGateway(t)

	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "boot-s3cret")
	t.Setenv("AGENT_REGISTRATION_ID", "tnt1.fm-operations-agent")

	deps, err := ConnectRuntimeDeps(context.Background(), &RuntimeDepsConfig{
		GatewayURL: url,
		TenantID:   "tnt1",
		AgentID:    "fm-operations-agent",
	})
	if err != nil {
		t.Fatalf("ConnectRuntimeDeps: %v", err)
	}
	defer deps.Close()

	if issueHits.Load() != 1 {
		t.Errorf("/auth/token/issue calls = %d, want 1", issueHits.Load())
	}
	if _, err := deps.Gateway.CallTool(context.Background(), "kg_get_entity", map[string]any{"entity_id": "e1"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := lastAuth.Load().(string); got != "Bearer minted-workload-jwt" {
		t.Errorf("tool call Authorization = %q, want the minted token", got)
	}
}

// TestConnectRuntimeDeps_BootstrapMintFatal verifies that a missing bootstrap
// credential is a fatal startup error (the agent must not serve without an
// identity), not a silent fallback.
func TestConnectRuntimeDeps_BootstrapMintFatal(t *testing.T) {
	url, _, _ := issueGateway(t)
	// kind=secret but the secret value is unset → BootstrapFromEnv errors.
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "")
	t.Setenv("AGENT_REGISTRATION_ID", "tnt1.a")

	if _, err := ConnectRuntimeDeps(context.Background(), &RuntimeDepsConfig{
		GatewayURL: url,
		TenantID:   "tnt1",
		AgentID:    "a",
	}); err == nil {
		t.Fatal("expected a fatal error when AGENT_BOOTSTRAP_KIND=secret but the secret is unset")
	}
}

// TestConnectRuntimeDeps_NoBootstrapNoToken verifies that with no bootstrap
// identity the agent mints nothing and sends no Authorization header, which
// the gateway refuses with 401. A token file left at the path the removed
// legacy mode read (AGENT_SERVICE_TOKEN_PATH) is not read (OGA-1134).
func TestConnectRuntimeDeps_NoBootstrapNoToken(t *testing.T) {
	url, issueHits, lastAuth := issueGateway(t)
	leftover := filepath.Join(t.TempDir(), "service-token")
	if err := os.WriteFile(leftover, []byte("legacy-file-token"), 0o600); err != nil {
		t.Fatalf("write leftover token: %v", err)
	}
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")
	t.Setenv("AGENT_SERVICE_TOKEN_PATH", leftover)

	deps, err := ConnectRuntimeDeps(context.Background(), &RuntimeDepsConfig{
		GatewayURL: url,
		TenantID:   "tnt1",
		AgentID:    "a",
	})
	if err != nil {
		t.Fatalf("ConnectRuntimeDeps: %v", err)
	}
	defer deps.Close()

	if issueHits.Load() != 0 {
		t.Errorf("/auth/token/issue calls = %d, want 0", issueHits.Load())
	}
	lastAuth.Store("unset")
	if _, err := deps.Gateway.CallTool(context.Background(), "kg_get_entity", map[string]any{"entity_id": "e1"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := lastAuth.Load().(string); got != "" {
		t.Errorf("tool call Authorization = %q, want none", got)
	}
}
