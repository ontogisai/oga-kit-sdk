package auth

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

// issueGateway stands up a fake gateway serving POST /auth/token/issue and
// counting its calls. Every other path is 404, as on the platform.
func issueGateway(t *testing.T) (url string, issueHits *int) {
	t.Helper()
	var iss int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/token/issue" {
			http.NotFound(w, r)
			return
		}
		iss++
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted", "expires_at": time.Now().Add(time.Hour)})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &iss
}

func TestNewTokenManagerFromEnv_BootstrapMint(t *testing.T) {
	url, issueHits := issueGateway(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "boot-s3cret")
	t.Setenv("AGENT_REGISTRATION_ID", "tnt1.built-env-tools-mcp")

	tm, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: url + "/"})
	if err != nil {
		t.Fatalf("NewTokenManagerFromEnv: %v", err)
	}
	if tm == nil {
		t.Fatal("expected a token manager when a bootstrap identity is configured")
	}
	defer tm.Stop()

	if *issueHits != 1 {
		t.Errorf("/auth/token/issue calls = %d, want 1", *issueHits)
	}
	if tm.Token() != "minted" {
		t.Errorf("Token() = %q, want the minted token", tm.Token())
	}
}

// AGENT_REGISTRATION_ID wins; the config's id is the fallback when it is unset.
func TestNewTokenManagerFromEnv_RegistrationIDFallback(t *testing.T) {
	ms := newMintServer(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "boot-s3cret")
	t.Setenv("AGENT_REGISTRATION_ID", "")

	// newMintServer answers on every path, so the derived issue URL reaches it.
	tm, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{
		GatewayURL: ms.URL, AgentRegistrationID: "tnt1.fallback-agent",
	})
	if err != nil {
		t.Fatalf("NewTokenManagerFromEnv: %v", err)
	}
	defer tm.Stop()
	if ms.lastBody["agent_registration_id"] != "tnt1.fallback-agent" {
		t.Errorf("agent_registration_id = %q, want the config fallback", ms.lastBody["agent_registration_id"])
	}
}

func TestNewTokenManagerFromEnv_BootstrapFatalWhenSecretMissing(t *testing.T) {
	url, _ := issueGateway(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "") // unset → BootstrapFromEnv errors
	t.Setenv("AGENT_REGISTRATION_ID", "tnt1.x")

	if _, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: url}); err == nil {
		t.Fatal("expected a fatal error when AGENT_BOOTSTRAP_KIND=secret but the secret is unset")
	}
}

// With no bootstrap identity there is no token manager, and a token file left
// at the path the removed legacy mode read (AGENT_SERVICE_TOKEN_PATH) is not
// read: nothing is minted and nothing is loaded (OGA-1134).
func TestNewTokenManagerFromEnv_NoBootstrapNoToken(t *testing.T) {
	url, issueHits := issueGateway(t)
	leftover := filepath.Join(t.TempDir(), "service-token")
	if err := os.WriteFile(leftover, []byte("legacy-file-token"), 0o600); err != nil {
		t.Fatalf("write leftover token: %v", err)
	}
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")
	t.Setenv("AGENT_SERVICE_TOKEN_PATH", leftover)

	tm, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: url})
	if err != nil {
		t.Fatalf("NewTokenManagerFromEnv: %v", err)
	}
	if tm != nil {
		tm.Stop()
		t.Fatal("expected no token manager without a bootstrap identity")
	}
	if *issueHits != 0 {
		t.Errorf("/auth/token/issue calls = %d, want 0", *issueHits)
	}
}
