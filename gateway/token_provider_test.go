package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestClient_TokenProviderDrivesAuthHeader verifies that once a token provider
// is wired, the client sends the provider's current value as the bearer — and
// picks up rotations (the provider returning a new value) without re-reading a
// file or restarting.
func TestClient_TokenProviderDrivesAuthHeader(t *testing.T) {
	var lastAuth atomic.Value
	lastAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow_id":"wf-1"}`))
	}))
	defer srv.Close()

	c := NewPlatformGatewayClient(srv.URL, "", "sgac1")

	// Rotating provider: returns token-A then token-B.
	var calls atomic.Int32
	c.SetTokenProvider(func() string {
		if calls.Add(1) == 1 {
			return "token-A"
		}
		return "token-B"
	})

	if _, err := c.post(context.Background(), "/workflow", map[string]any{"x": 1}); err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if got := lastAuth.Load().(string); got != "Bearer token-A" {
		t.Errorf("first request auth = %q, want %q", got, "Bearer token-A")
	}

	if _, err := c.post(context.Background(), "/workflow", map[string]any{"x": 2}); err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if got := lastAuth.Load().(string); got != "Bearer token-B" {
		t.Errorf("second request auth = %q, want rotated %q", got, "Bearer token-B")
	}
}

// TestClient_NoProviderNoToken verifies the client sends no Authorization
// header when neither a provider nor a token path is configured (dev-mode
// tokenless path is preserved).
func TestClient_NoProviderNoToken(t *testing.T) {
	var hadAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadAuth.Store(r.Header.Get("Authorization") != "")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewPlatformGatewayClient(srv.URL, "", "sgac1")
	if _, err := c.post(context.Background(), "/workflow", nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	if hadAuth.Load() {
		t.Error("expected no Authorization header when tokenless")
	}
}
