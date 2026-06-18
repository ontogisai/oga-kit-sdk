package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenManager handles service token lifecycle for agent sidecars.
// It performs sliding renewal at 50% TTL, atomic file rotation, and
// exponential backoff retry on refresh failure.
type TokenManager struct {
	tokenPath  string
	refreshURL string
	client     *http.Client

	// Bootstrap-mint mode (OGA-404). When bootstrap != nil the manager mints
	// its workload token from a durable bootstrap identity at boot and on every
	// renewal — no token file, no refresh-with-old-token. When nil, the legacy
	// file + /auth/token/refresh path is used.
	bootstrap  BootstrapProvider
	issueURL   string
	agentRegID string

	mu        sync.RWMutex
	current   string
	expiresAt time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// TokenManagerConfig configures the token manager.
type TokenManagerConfig struct {
	// TokenPath is the filesystem path where the token is stored (legacy
	// file+refresh mode only; ignored in bootstrap-mint mode).
	TokenPath string

	// RefreshURL is the gateway endpoint for token refresh (legacy mode).
	// Typically: PLATFORM_GATEWAY_URL + "/auth/token/refresh"
	RefreshURL string

	// Bootstrap, when set, switches the manager to OGA-404 mint mode: the
	// workload token is minted from this durable bootstrap identity rather than
	// loaded from a file. IssueURL and AgentRegistrationID are then required.
	Bootstrap BootstrapProvider

	// IssueURL is the gateway mint endpoint (bootstrap mode).
	// Typically: PLATFORM_GATEWAY_URL + "/auth/token/issue"
	IssueURL string

	// AgentRegistrationID is the "{tenant}.{name}" identity the mint endpoint
	// authorizes the bootstrap credential against (bootstrap mode).
	AgentRegistrationID string
}

// NewTokenManager creates a new token manager and starts the renewal goroutine.
//
// Two modes:
//   - Bootstrap-mint (OGA-404): set cfg.Bootstrap + cfg.IssueURL +
//     cfg.AgentRegistrationID. The token is minted from the bootstrap identity
//     at boot and on each renewal; nothing is read from or written to disk.
//   - Legacy file+refresh: set cfg.TokenPath + cfg.RefreshURL. The initial
//     token is loaded from the file and rotated via /auth/token/refresh.
func NewTokenManager(ctx context.Context, cfg *TokenManagerConfig) (*TokenManager, error) {
	tm := &TokenManager{
		client: &http.Client{Timeout: 30 * time.Second},
		stopCh: make(chan struct{}),
	}

	if cfg.Bootstrap != nil {
		// Bootstrap-mint mode.
		if cfg.IssueURL == "" {
			return nil, fmt.Errorf("IssueURL is required in bootstrap mode")
		}
		if cfg.AgentRegistrationID == "" {
			return nil, fmt.Errorf("AgentRegistrationID is required in bootstrap mode")
		}
		tm.bootstrap = cfg.Bootstrap
		tm.issueURL = cfg.IssueURL
		tm.agentRegID = cfg.AgentRegistrationID

		// Mint the initial token before serving. A failure here is fatal: the
		// agent cannot prove identity and must not start serving with no token.
		if err := tm.mint(ctx); err != nil {
			return nil, fmt.Errorf("initial workload token mint: %w", err)
		}
	} else {
		// Legacy file+refresh mode.
		if cfg.TokenPath == "" {
			return nil, fmt.Errorf("TokenPath is required")
		}
		if cfg.RefreshURL == "" {
			return nil, fmt.Errorf("RefreshURL is required")
		}
		tm.tokenPath = cfg.TokenPath
		tm.refreshURL = cfg.RefreshURL
		if err := tm.loadFromFile(); err != nil {
			slog.Warn("initial token load failed", "error", err, "path", cfg.TokenPath)
		}
	}

	// Start renewal goroutine
	tm.wg.Add(1)
	go tm.renewalLoop(ctx)

	return tm, nil
}

// Token returns the current valid token.
func (tm *TokenManager) Token() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.current
}

// Stop stops the renewal goroutine.
func (tm *TokenManager) Stop() {
	close(tm.stopCh)
	tm.wg.Wait()
}

func (tm *TokenManager) loadFromFile() error {
	data, err := os.ReadFile(tm.tokenPath)
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}

	token := strings.TrimSpace(string(data))
	if token == "" {
		return fmt.Errorf("token file is empty")
	}

	tm.mu.Lock()
	tm.current = token
	// The token file is the compact JSON of the platform ServiceToken; parse
	// its expires_at so the renewal loop schedules against the real TTL rather
	// than a 1h guess. Fall back to now+1h only when the field is unparseable.
	if exp := parseTokenExpiry(token); !exp.IsZero() {
		tm.expiresAt = exp
	} else if tm.expiresAt.IsZero() {
		tm.expiresAt = time.Now().Add(1 * time.Hour)
	}
	tm.mu.Unlock()

	return nil
}

// parseTokenExpiry extracts expires_at from the compact ServiceToken JSON the
// gateway issues. Returns the zero time when the token is not JSON or has no
// expires_at (e.g. an opaque dev token).
func parseTokenExpiry(token string) time.Time {
	var st struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(token), &st); err != nil {
		return time.Time{}
	}
	return st.ExpiresAt
}

func (tm *TokenManager) renewalLoop(ctx context.Context) {
	defer tm.wg.Done()

	for {
		tm.mu.RLock()
		expiresAt := tm.expiresAt
		tm.mu.RUnlock()

		// Renew at 50% TTL
		ttl := time.Until(expiresAt)
		renewAt := ttl / 2
		if renewAt < 1*time.Minute {
			renewAt = 1 * time.Minute
		}

		select {
		case <-time.After(renewAt):
			if err := tm.renew(ctx); err != nil {
				slog.Error("token renewal failed", "error", err)
			}
		case <-tm.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// renew rotates the workload token using the configured mode: bootstrap mint
// (OGA-404) when a bootstrap provider is set, otherwise the legacy refresh.
func (tm *TokenManager) renew(ctx context.Context) error {
	if tm.bootstrap != nil {
		return tm.mint(ctx)
	}
	return tm.refresh(ctx)
}

// acquire runs attemptFn with exponential backoff (1,2,4,8,16s; 5 attempts).
// On success it adopts the token in memory FIRST (the live identity the gateway
// client serves outbound requests from) and, when persist is true, writes it
// best-effort to the token file. Shared by the bootstrap-mint and legacy
// refresh paths.
func (tm *TokenManager) acquire(ctx context.Context, label string, persist bool, attemptFn func(context.Context) (string, time.Time, error)) error {
	delay := 1 * time.Second
	const maxAttempts = 5

	var lastErr error
	for attempt := range maxAttempts {
		newToken, expiresAt, err := attemptFn(ctx)
		if err == nil {
			tm.mu.Lock()
			tm.current = newToken
			tm.expiresAt = expiresAt
			tm.mu.Unlock()

			// Persistence is best-effort and only used by the legacy file mode.
			// A write failure (e.g. read-only mount) must NOT discard the
			// freshly-issued token (OGA-400).
			if persist {
				if werr := tm.atomicWriteToken(newToken); werr != nil {
					slog.Warn("token "+label+" in memory but file persist failed (continuing)",
						"error", werr, "path", tm.tokenPath)
				}
			}

			slog.Info("token "+label, "expires_at", expiresAt, "attempt", attempt+1)
			return nil
		}

		lastErr = err
		slog.Warn("token "+label+" attempt failed",
			"attempt", attempt+1,
			"max_attempts", maxAttempts,
			"error", err,
			"retry_in", delay,
		)

		select {
		case <-time.After(delay):
			delay *= 2 // exponential backoff
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("token %s failed after %d attempts: %w", label, maxAttempts, lastErr)
}

// mint obtains a fresh workload token from the gateway mint endpoint using the
// bootstrap identity. No file is read or written — the token lives only in
// memory. The gateway never needs the prior workload token (the bootstrap
// identity is authoritative), so this path has no "old token expired" failure
// mode (OGA-404).
func (tm *TokenManager) mint(ctx context.Context) error {
	return tm.acquire(ctx, "minted", false, tm.doMint)
}

func (tm *TokenManager) refresh(ctx context.Context) error {
	return tm.acquire(ctx, "refreshed", true, func(ctx context.Context) (string, time.Time, error) {
		tm.mu.RLock()
		currentToken := tm.current
		tm.mu.RUnlock()
		return tm.doRefresh(ctx, currentToken)
	})
}

// doMint POSTs the bootstrap identity to the gateway mint endpoint and parses
// the {token, expires_at} response. For the k8s_sa kind the SA token travels in
// the body; for the secret kind it travels in the Authorization header (never
// the body), matching the gateway's handleTokenIssue contract.
func (tm *TokenManager) doMint(ctx context.Context) (string, time.Time, error) {
	kind, cred, err := tm.bootstrap.Token(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read bootstrap identity: %w", err)
	}

	reqBody := map[string]string{
		"bootstrap_kind":        kind,
		"agent_registration_id": tm.agentRegID,
	}
	if kind == BootstrapKindK8sSA {
		reqBody["bootstrap_token"] = cred
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal mint request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.issueURL, strings.NewReader(string(payload)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if kind == BootstrapKindSecret {
		req.Header.Set("Authorization", "Bearer "+cred)
	}

	resp, err := tm.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read mint response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("mint returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("parse mint response: %w", err)
	}
	if result.Token == "" {
		return "", time.Time{}, fmt.Errorf("mint response missing token")
	}
	return result.Token, result.ExpiresAt, nil
}

func (tm *TokenManager) doRefresh(ctx context.Context, currentToken string) (string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.refreshURL, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+currentToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("refresh returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("parse refresh response: %w", err)
	}

	if result.Token == "" {
		return "", time.Time{}, fmt.Errorf("refresh response missing token")
	}

	return result.Token, result.ExpiresAt, nil
}

func (tm *TokenManager) atomicWriteToken(token string) error {
	dir := filepath.Dir(tm.tokenPath)
	tmpFile, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.WriteString(token); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, tm.tokenPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp to token: %w", err)
	}

	return nil
}
