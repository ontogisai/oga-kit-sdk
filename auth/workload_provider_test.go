package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// WorkloadTokenProviderFromEnv mints a workload token from the secret bootstrap
// identity and returns a provider yielding it.
func TestWorkloadTokenProviderFromEnv_Secret(t *testing.T) {
	ms := newMintServer(t)
	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "s3cret-bootstrap")
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.brick-ontology-loader")

	provider, stop, err := WorkloadTokenProviderFromEnv(context.Background(), ms.URL)
	if err != nil {
		t.Fatalf("WorkloadTokenProviderFromEnv: %v", err)
	}
	if provider == nil {
		t.Fatal("expected a non-nil provider when a bootstrap identity is configured")
	}
	defer stop()

	if got := provider(); got != ms.tokenValue {
		t.Errorf("provider() = %q, want %q", got, ms.tokenValue)
	}
	if ms.lastAuth != "Bearer s3cret-bootstrap" {
		t.Errorf("mint Authorization = %q, want Bearer s3cret-bootstrap", ms.lastAuth)
	}
	if ms.lastBody["agent_registration_id"] != "sgac1.brick-ontology-loader" {
		t.Errorf("agent_registration_id = %q", ms.lastBody["agent_registration_id"])
	}
}

// With no bootstrap identity configured, the helper returns (nil, noop, nil) so
// the caller can fall back to a token file / dev no-auth mode.
func TestWorkloadTokenProviderFromEnv_NoBootstrap(t *testing.T) {
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")

	provider, stop, err := WorkloadTokenProviderFromEnv(context.Background(), "http://localhost:8050")
	if err != nil {
		t.Fatalf("WorkloadTokenProviderFromEnv: %v", err)
	}
	if provider != nil {
		t.Error("expected nil provider when AGENT_BOOTSTRAP_KIND is unset")
	}
	if stop == nil {
		t.Error("stop must be a non-nil no-op even when no provider is built")
	}
	stop() // must not panic
}

// A bootstrap identity that the gateway rejects (e.g. 401) is a fatal error —
// the helper surfaces it rather than returning a tokenless provider.
func TestWorkloadTokenProviderFromEnv_MintFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "wrong-secret")
	t.Setenv("AGENT_REGISTRATION_ID", "sgac1.brick-ontology-loader")

	provider, _, err := WorkloadTokenProviderFromEnv(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected a fatal error when the initial mint fails")
	}
	if provider != nil {
		t.Error("provider must be nil on mint failure (never run tokenless)")
	}
}
