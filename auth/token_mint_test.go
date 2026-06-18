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

// mintServer is an httptest gateway mirroring POST /auth/token/issue. It
// captures the last request so tests can assert the wire contract.
type mintServer struct {
	*httptest.Server
	lastBody   map[string]string
	lastAuth   string
	tokenValue string
	expiresAt  time.Time
}

func newMintServer(t *testing.T) *mintServer {
	t.Helper()
	ms := &mintServer{
		tokenValue: "minted-workload-jwt",
		expiresAt:  time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second),
	}
	ms.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.lastAuth = r.Header.Get("Authorization")
		ms.lastBody = map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&ms.lastBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      ms.tokenValue,
			"expires_at": ms.expiresAt,
		})
	}))
	t.Cleanup(ms.Close)
	return ms
}

func TestNewTokenManager_BootstrapMode_K8sSA(t *testing.T) {
	ms := newMintServer(t)

	// Projected SA token file (read by K8sSABootstrap each mint).
	dir := t.TempDir()
	saPath := filepath.Join(dir, "token")
	if err := os.WriteFile(saPath, []byte("projected-sa-jwt\n"), 0o600); err != nil {
		t.Fatalf("write SA token: %v", err)
	}

	tm, err := NewTokenManager(context.Background(), &TokenManagerConfig{
		Bootstrap:           K8sSABootstrap{Path: saPath},
		IssueURL:            ms.URL,
		AgentRegistrationID: "sgac1.fm-operations-agent",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	defer tm.Stop()

	if got := tm.Token(); got != ms.tokenValue {
		t.Errorf("Token() = %q, want %q", got, ms.tokenValue)
	}
	// Wire contract: k8s_sa sends the SA token in the body, not the header.
	if ms.lastBody["bootstrap_kind"] != "k8s_sa" {
		t.Errorf("bootstrap_kind = %q", ms.lastBody["bootstrap_kind"])
	}
	if ms.lastBody["bootstrap_token"] != "projected-sa-jwt" {
		t.Errorf("bootstrap_token = %q (want trimmed SA token in body)", ms.lastBody["bootstrap_token"])
	}
	if ms.lastBody["agent_registration_id"] != "sgac1.fm-operations-agent" {
		t.Errorf("agent_registration_id = %q", ms.lastBody["agent_registration_id"])
	}
	if ms.lastAuth != "" {
		t.Errorf("k8s_sa must NOT send an Authorization header, got %q", ms.lastAuth)
	}
}

func TestNewTokenManager_BootstrapMode_Secret(t *testing.T) {
	ms := newMintServer(t)

	tm, err := NewTokenManager(context.Background(), &TokenManagerConfig{
		Bootstrap:           SecretBootstrap{Secret: "s3cret-bootstrap"},
		IssueURL:            ms.URL,
		AgentRegistrationID: "sgac1.fm-operations-agent",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	defer tm.Stop()

	if got := tm.Token(); got != ms.tokenValue {
		t.Errorf("Token() = %q", got)
	}
	// Wire contract: secret travels in the Authorization header, never the body.
	if ms.lastAuth != "Bearer s3cret-bootstrap" {
		t.Errorf("Authorization = %q, want Bearer s3cret-bootstrap", ms.lastAuth)
	}
	if _, present := ms.lastBody["bootstrap_token"]; present {
		t.Error("secret kind must NOT put the credential in the body")
	}
	if ms.lastBody["bootstrap_kind"] != "secret" {
		t.Errorf("bootstrap_kind = %q", ms.lastBody["bootstrap_kind"])
	}
}

func TestNewTokenManager_BootstrapMode_Validation(t *testing.T) {
	tests := []struct {
		name string
		cfg  *TokenManagerConfig
	}{
		{"missing IssueURL", &TokenManagerConfig{Bootstrap: SecretBootstrap{Secret: "x"}, AgentRegistrationID: "t.a"}},
		{"missing AgentRegistrationID", &TokenManagerConfig{Bootstrap: SecretBootstrap{Secret: "x"}, IssueURL: "http://x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTokenManager(context.Background(), tt.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNewTokenManager_BootstrapMode_InitialMintFailsFatally(t *testing.T) {
	// Gateway returns 401 — the initial mint must fail and NewTokenManager must
	// return an error (the agent cannot start serving without a token).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bootstrap_rejected"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := NewTokenManager(ctx, &TokenManagerConfig{
		Bootstrap:           SecretBootstrap{Secret: "x"},
		IssueURL:            srv.URL,
		AgentRegistrationID: "t.a",
	})
	if err == nil {
		t.Fatal("expected fatal error when initial mint is rejected")
	}
}

func TestK8sSABootstrap_ReadsFreshEachCall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	_ = os.WriteFile(p, []byte("v1"), 0o600)
	b := K8sSABootstrap{Path: p}

	kind, tok, err := b.Token(context.Background())
	if err != nil || kind != BootstrapKindK8sSA || tok != "v1" {
		t.Fatalf("got (%q,%q,%v)", kind, tok, err)
	}
	// kubelet rotates the file; the next read must see the new value.
	_ = os.WriteFile(p, []byte("v2"), 0o600)
	_, tok2, _ := b.Token(context.Background())
	if tok2 != "v2" {
		t.Errorf("expected fresh read v2, got %q", tok2)
	}
}

func TestBootstrapFromEnv(t *testing.T) {
	t.Setenv("AGENT_BOOTSTRAP_KIND", "")
	if p, err := BootstrapFromEnv(); p != nil || err != nil {
		t.Errorf("unset kind → (nil,nil), got (%v,%v)", p, err)
	}

	t.Setenv("AGENT_BOOTSTRAP_KIND", "secret")
	t.Setenv("AGENT_BOOTSTRAP_SECRET", "abc")
	p, err := BootstrapFromEnv()
	if err != nil {
		t.Fatalf("secret env: %v", err)
	}
	if _, tok, _ := p.Token(context.Background()); tok != "abc" {
		t.Errorf("secret token = %q", tok)
	}

	t.Setenv("AGENT_BOOTSTRAP_KIND", "k8s_sa")
	t.Setenv("AGENT_SA_TOKEN_PATH", "")
	if _, err := BootstrapFromEnv(); err == nil {
		t.Error("k8s_sa without AGENT_SA_TOKEN_PATH should error")
	}
}
