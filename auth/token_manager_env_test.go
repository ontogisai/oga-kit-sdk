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

// issueRefreshGateway stands up a fake gateway capturing /auth/token/issue and
// /auth/token/refresh hit counts so the env helper's mode selection can be
// asserted.
func issueRefreshGateway(t *testing.T) (url string, issueHits, refreshHits *int) {
	t.Helper()
	var iss, ref int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"token": `{"expires_at":"2030-01-01T00:00:00Z"}`, "expires_at": time.Now().Add(time.Hour)}
		switch r.URL.Path {
		case "/auth/token/issue":
			iss++
		case "/auth/token/refresh":
			ref++
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &iss, &ref
}

func TestNewTokenManagerFromEnv_BootstrapMint(t *testing.T) {
	url, issueHits, refreshHits := issueRefreshGateway(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "boot-s3cret")
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.built-env-tools-mcp")

	tm, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: url})
	if err != nil {
		t.Fatalf("NewTokenManagerFromEnv: %v", err)
	}
	if tm == nil {
		t.Fatal("expected a token manager in bootstrap mode")
	}
	defer tm.Stop()

	if *issueHits == 0 {
		t.Error("expected an /auth/token/issue mint call")
	}
	if *refreshHits != 0 {
		t.Errorf("bootstrap mode must not call /auth/token/refresh, got %d", *refreshHits)
	}
	if tm.Token() == "" {
		t.Error("expected a non-empty minted token")
	}
}

func TestNewTokenManagerFromEnv_BootstrapFatalWhenSecretMissing(t *testing.T) {
	url, _, _ := issueRefreshGateway(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "") // unset → BootstrapFromEnv errors
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.x")

	if _, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: url}); err == nil {
		t.Fatal("expected a fatal error when AGENT_BOOTSTRAP_KIND=secret but the secret is unset")
	}
}

func TestNewTokenManagerFromEnv_LegacyFile(t *testing.T) {
	url, issueHits, _ := issueRefreshGateway(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "") // no bootstrap env

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(`{"expires_at":"2030-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	tm, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: url, TokenPath: tokenPath})
	if err != nil {
		t.Fatalf("NewTokenManagerFromEnv (legacy): %v", err)
	}
	if tm == nil {
		t.Fatal("expected a token manager in legacy file mode")
	}
	defer tm.Stop()
	if *issueHits != 0 {
		t.Errorf("legacy mode must not call /auth/token/issue, got %d", *issueHits)
	}
}

func TestNewTokenManagerFromEnv_None(t *testing.T) {
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")
	tm, err := NewTokenManagerFromEnv(context.Background(), EnvTokenManagerConfig{GatewayURL: "http://localhost:8050"})
	if err != nil {
		t.Fatalf("NewTokenManagerFromEnv (none): %v", err)
	}
	if tm != nil {
		t.Error("expected nil token manager when neither bootstrap nor token path is set")
		tm.Stop()
	}
}
