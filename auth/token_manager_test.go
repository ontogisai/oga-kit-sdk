package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortMintRetry makes the mint retry wait 1 ms instead of 1 s for the test.
func shortMintRetry(t *testing.T) {
	t.Helper()
	prev := mintFirstDelay
	mintFirstDelay = time.Millisecond
	t.Cleanup(func() { mintFirstDelay = prev })
}

// Every field of the config is required; there is no other mode.
func TestNewTokenManager_ConfigIsRequired(t *testing.T) {
	tests := []struct {
		name string
		cfg  *TokenManagerConfig
	}{
		{"nil config", nil},
		{"no bootstrap identity", &TokenManagerConfig{IssueURL: "http://x", AgentRegistrationID: "t.a"}},
		{"no issue URL", &TokenManagerConfig{Bootstrap: SecretBootstrap{Secret: "x"}, AgentRegistrationID: "t.a"}},
		{"no registration id", &TokenManagerConfig{Bootstrap: SecretBootstrap{Secret: "x"}, IssueURL: "http://x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTokenManager(context.Background(), tt.cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// A first mint the gateway keeps refusing is tried mintAttempts times, then
// returned as an error that names the gateway's answer.
func TestNewTokenManager_FirstMintFailureIsAnError(t *testing.T) {
	shortMintRetry(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"OGA-AGNT-AUTH-4601"}`))
	}))
	t.Cleanup(srv.Close)

	tm, err := NewTokenManager(context.Background(), &TokenManagerConfig{
		Bootstrap:           SecretBootstrap{Secret: "wrong"},
		IssueURL:            srv.URL,
		AgentRegistrationID: "tnt1.a",
	})
	if err == nil {
		tm.Stop()
		t.Fatal("expected an error when every mint is refused")
	}
	if got := hits.Load(); got != mintAttempts {
		t.Errorf("mint attempts = %d, want %d", got, mintAttempts)
	}
	for _, want := range []string{"after 5 attempts", "mint returned 401", "OGA-AGNT-AUTH-4601"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// A mint that fails and then succeeds within the retry budget succeeds.
func TestNewTokenManager_MintRetriesThenSucceeds(t *testing.T) {
	shortMintRetry(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "third-time", "expires_at": time.Now().Add(12 * time.Hour)})
	}))
	t.Cleanup(srv.Close)

	tm, err := NewTokenManager(context.Background(), &TokenManagerConfig{
		Bootstrap:           SecretBootstrap{Secret: "s"},
		IssueURL:            srv.URL,
		AgentRegistrationID: "tnt1.a",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	defer tm.Stop()
	if got := tm.Token(); got != "third-time" {
		t.Errorf("Token() = %q, want the token from the third attempt", got)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("mint attempts = %d, want 3", got)
	}
}

// Renewal mints again with the bootstrap identity as it is now (a rotated
// ServiceAccount token is picked up), never with the previous workload token,
// and the new token replaces the old one.
func TestTokenManager_RenewalMintsWithCurrentBootstrapIdentity(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]string
	var auths []string
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "workload-" + strconv.Itoa(int(n.Add(1))),
			"expires_at": time.Now().Add(12 * time.Hour),
		})
	}))
	t.Cleanup(srv.Close)

	saPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(saPath, []byte("sa-v1"), 0o600); err != nil {
		t.Fatalf("write SA token: %v", err)
	}
	tm, err := NewTokenManager(context.Background(), &TokenManagerConfig{
		Bootstrap:           K8sSABootstrap{Path: saPath},
		IssueURL:            srv.URL,
		AgentRegistrationID: "tnt1.a",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	defer tm.Stop()
	if got := tm.Token(); got != "workload-1" {
		t.Fatalf("Token() = %q, want workload-1", got)
	}

	// kubelet rotates the projected token; the next mint must present it.
	if err := os.WriteFile(saPath, []byte("sa-v2"), 0o600); err != nil {
		t.Fatalf("rotate SA token: %v", err)
	}
	if err := tm.mint(context.Background()); err != nil {
		t.Fatalf("renewal mint: %v", err)
	}
	if got := tm.Token(); got != "workload-2" {
		t.Errorf("Token() after renewal = %q, want workload-2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("mint calls = %d, want 2", len(bodies))
	}
	if bodies[1]["bootstrap_token"] != "sa-v2" {
		t.Errorf("renewal bootstrap_token = %q, want the rotated sa-v2", bodies[1]["bootstrap_token"])
	}
	if auths[1] != "" {
		t.Errorf("renewal sent Authorization %q; a k8s_sa mint sends none (the old workload token is never presented)", auths[1])
	}
}

// Stop is safe to call more than once: mcptools.Runtime.Close (which
// ListenAndServe also calls) and the gateway env client's Closer both end in
// it. A second call used to panic with "close of closed channel".
func TestTokenManager_StopTwice(t *testing.T) {
	ms := newMintServer(t)
	tm, err := NewTokenManager(context.Background(), &TokenManagerConfig{
		Bootstrap:           SecretBootstrap{Secret: "s"},
		IssueURL:            ms.URL,
		AgentRegistrationID: "tnt1.a",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	tm.Stop()
	tm.Stop()
}

// Renewal ends when the context passed to NewTokenManager ends, and Stop
// afterwards still returns.
func TestTokenManager_RenewalEndsWithContext(t *testing.T) {
	ms := newMintServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	tm, err := NewTokenManager(ctx, &TokenManagerConfig{
		Bootstrap:           SecretBootstrap{Secret: "s"},
		IssueURL:            ms.URL,
		AgentRegistrationID: "tnt1.a",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		tm.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal goroutine still running after its context ended")
	}
	tm.Stop()
}
