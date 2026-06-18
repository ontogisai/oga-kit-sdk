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

// writeTokenFile writes a compact ServiceToken-shaped JSON to a temp file and
// returns the path.
func writeTokenFile(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "service-token")
	tok := map[string]any{
		"token_id":   "tok-1",
		"agent_id":   "sgac1.fm-operations-agent",
		"tenant_id":  "sgac1",
		"expires_at": expiresAt.Format(time.RFC3339Nano),
		"signature":  "sig",
	}
	b, _ := json.Marshal(tok)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestParseTokenExpiry(t *testing.T) {
	exp := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
	tok := map[string]any{"expires_at": exp.Format(time.RFC3339Nano)}
	b, _ := json.Marshal(tok)

	got := parseTokenExpiry(string(b))
	if !got.Equal(exp) {
		t.Errorf("parseTokenExpiry = %v, want %v", got, exp)
	}

	if !parseTokenExpiry("not-json").IsZero() {
		t.Error("non-JSON token should yield zero expiry")
	}
}

func TestTokenManager_LoadParsesExpiry(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	path := writeTokenFile(t, exp)

	tm := &TokenManager{tokenPath: path}
	if err := tm.loadFromFile(); err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if !tm.expiresAt.Equal(exp) {
		t.Errorf("expiresAt = %v, want parsed %v (not the 1h default)", tm.expiresAt, exp)
	}
}

// TestTokenManager_RefreshRoundTrip exercises the full refresh against a mock
// gateway that mirrors the platform contract: current token in the
// Authorization header, response {token, expires_at}.
func TestTokenManager_RefreshRoundTrip(t *testing.T) {
	newExpiry := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      `{"token_id":"tok-2","agent_id":"sgac1.fm-operations-agent","tenant_id":"sgac1","expires_at":"` + newExpiry.Format(time.RFC3339Nano) + `","signature":"sig2"}`,
			"expires_at": newExpiry,
		})
	}))
	defer srv.Close()

	path := writeTokenFile(t, time.Now().Add(2*time.Minute))
	tm := &TokenManager{tokenPath: path, refreshURL: srv.URL, client: srv.Client()}
	if err := tm.loadFromFile(); err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}

	if err := tm.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if gotAuth == "" || gotAuth[:7] != "Bearer " {
		t.Errorf("refresh did not send Bearer auth header, got %q", gotAuth)
	}
	// New token is now current and persisted to the file.
	if got := tm.Token(); parseTokenExpiry(got) != newExpiry {
		t.Errorf("current token expiry = %v, want %v", parseTokenExpiry(got), newExpiry)
	}
	onDisk, _ := os.ReadFile(path)
	if parseTokenExpiry(string(onDisk)) != newExpiry {
		t.Error("rotated token was not written back to the file")
	}
}

// TestTokenManager_RefreshAdoptsTokenWhenPersistFails is the OGA-400 regression
// guard. Kit agent sidecars run with the credential dir bind-mounted read-only,
// so atomicWriteToken fails. Because the gateway shortens the OLD token's
// expiry the instant it issues the new one, the SDK MUST still adopt the new
// token in memory even when it cannot persist it — otherwise the next renewal
// presents the burned old token and the gateway 401s, bricking the agent.
func TestTokenManager_RefreshAdoptsTokenWhenPersistFails(t *testing.T) {
	newExpiry := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      `{"token_id":"tok-2","agent_id":"sgac1.fm-operations-agent","tenant_id":"sgac1","expires_at":"` + newExpiry.Format(time.RFC3339Nano) + `","signature":"sig2"}`,
			"expires_at": newExpiry,
		})
	}))
	defer srv.Close()

	// Load the initial token from a real file, then redirect tokenPath into a
	// non-existent directory so atomicWriteToken (os.CreateTemp on the dir)
	// fails — simulating the read-only credential mount.
	loadPath := writeTokenFile(t, time.Now().Add(2*time.Minute))
	tm := &TokenManager{tokenPath: loadPath, refreshURL: srv.URL, client: srv.Client()}
	if err := tm.loadFromFile(); err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	oldToken := tm.Token()
	tm.tokenPath = filepath.Join(t.TempDir(), "does-not-exist", "service-token")

	// Refresh must succeed (no error) despite the unwritable path.
	if err := tm.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error on persist failure, want nil: %v", err)
	}

	// The in-memory token MUST be the rotated one, not the burned old token.
	got := tm.Token()
	if got == oldToken {
		t.Fatal("refresh kept the old token after a persist failure — agent would be locked out")
	}
	if parseTokenExpiry(got) != newExpiry {
		t.Errorf("current token expiry = %v, want rotated %v", parseTokenExpiry(got), newExpiry)
	}
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if !tm.expiresAt.Equal(newExpiry) {
		t.Errorf("expiresAt = %v, want %v (renewal must schedule against the new TTL)", tm.expiresAt, newExpiry)
	}
}
