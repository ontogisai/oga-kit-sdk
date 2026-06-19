package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// issueGateway is an httptest gateway capturing POST /auth/token/issue and
// /auth/token/refresh so the chassis token-mode selection can be asserted.
func issueGateway(t *testing.T) (url string, issueHits, refreshHits *int) {
	t.Helper()
	var iss, ref int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token/issue":
			iss++
		case "/auth/token/refresh":
			ref++
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "minted-workload-jwt",
			"expires_at": time.Now().Add(12 * time.Hour).UTC(),
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &iss, &ref
}

// TestConnectRuntimeDeps_BootstrapMintMode verifies the OGA-404 chassis wiring:
// when the manager-injected AGENT_BOOTSTRAP_KIND env is present, the chassis
// mints the workload token from /auth/token/issue (not the legacy refresh path)
// and never requires a token file.
func TestConnectRuntimeDeps_BootstrapMintMode(t *testing.T) {
	url, issueHits, refreshHits := issueGateway(t)

	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "boot-s3cret")
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.fm-operations-agent")

	deps, err := ConnectRuntimeDeps(context.Background(), &RuntimeDepsConfig{
		GatewayURL: url,
		TenantID:   "sgac1",
		AgentID:    "fm-operations-agent",
		// No TokenPath — bootstrap mode must not require a file.
	})
	if err != nil {
		t.Fatalf("ConnectRuntimeDeps (bootstrap mode): %v", err)
	}
	defer deps.Close()

	if *issueHits == 0 {
		t.Error("expected an /auth/token/issue mint call in bootstrap mode")
	}
	if *refreshHits != 0 {
		t.Errorf("bootstrap mode must not call /auth/token/refresh, got %d", *refreshHits)
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
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.a")

	if _, err := ConnectRuntimeDeps(context.Background(), &RuntimeDepsConfig{
		GatewayURL: url,
		TenantID:   "sgac1",
		AgentID:    "a",
	}); err == nil {
		t.Fatal("expected a fatal error when AGENT_BOOTSTRAP_KIND=secret but the secret is unset")
	}
}

// TestConnectRuntimeDeps_LegacyFallback verifies that with no bootstrap env and
// a token file present, the chassis uses the legacy file+refresh path.
func TestConnectRuntimeDeps_LegacyFallback(t *testing.T) {
	url, issueHits, _ := issueGateway(t)
	// Ensure no bootstrap env leaks in from the environment.
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "service-token")
	if err := os.WriteFile(tokenPath, []byte(`{"token_id":"t","agent_id":"a","tenant_id":"sgac1"}`), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	deps, err := ConnectRuntimeDeps(context.Background(), &RuntimeDepsConfig{
		GatewayURL: url,
		TenantID:   "sgac1",
		AgentID:    "a",
		TokenPath:  tokenPath,
	})
	if err != nil {
		t.Fatalf("ConnectRuntimeDeps (legacy mode): %v", err)
	}
	defer deps.Close()

	if *issueHits != 0 {
		t.Errorf("legacy mode must not call /auth/token/issue, got %d", *issueHits)
	}
}
