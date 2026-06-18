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

	mu        sync.RWMutex
	current   string
	expiresAt time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// TokenManagerConfig configures the token manager.
type TokenManagerConfig struct {
	// TokenPath is the filesystem path where the token is stored.
	TokenPath string

	// RefreshURL is the gateway endpoint for token refresh.
	// Typically: PLATFORM_GATEWAY_URL + "/auth/token/refresh"
	RefreshURL string
}

// NewTokenManager creates a new token manager and starts the renewal goroutine.
func NewTokenManager(ctx context.Context, cfg *TokenManagerConfig) (*TokenManager, error) {
	if cfg.TokenPath == "" {
		return nil, fmt.Errorf("TokenPath is required")
	}
	if cfg.RefreshURL == "" {
		return nil, fmt.Errorf("RefreshURL is required")
	}

	tm := &TokenManager{
		tokenPath:  cfg.TokenPath,
		refreshURL: cfg.RefreshURL,
		client:     &http.Client{Timeout: 30 * time.Second},
		stopCh:     make(chan struct{}),
	}

	// Load initial token
	if err := tm.loadFromFile(); err != nil {
		slog.Warn("initial token load failed", "error", err, "path", cfg.TokenPath)
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
			if err := tm.refresh(ctx); err != nil {
				slog.Error("token refresh failed", "error", err)
			}
		case <-tm.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (tm *TokenManager) refresh(ctx context.Context) error {
	tm.mu.RLock()
	currentToken := tm.current
	tm.mu.RUnlock()

	// Exponential backoff: 1s, 2s, 4s, 8s, 16s (5 attempts)
	delay := 1 * time.Second
	maxAttempts := 5

	var lastErr error
	for attempt := range maxAttempts {
		newToken, expiresAt, err := tm.doRefresh(ctx, currentToken)
		if err == nil {
			// Adopt the rotated token in memory FIRST. This is the live identity
			// the gateway client serves outbound requests from (via the token
			// provider). The gateway shortens the OLD token's expiry to
			// now+overlap the instant it issues the new one, so we MUST switch
			// to the new token even if we cannot persist it — otherwise the next
			// renewal presents the burned old token and the gateway 401s
			// ("old token invalid: token has expired"), bricking the agent
			// (OGA-400).
			tm.mu.Lock()
			tm.current = newToken
			tm.expiresAt = expiresAt
			tm.mu.Unlock()

			// Persistence is best-effort. The file only lets a process restart
			// resume without a cold re-fetch; a write failure (e.g. the
			// read-only credential mount kit sidecars run with) must NOT discard
			// the freshly-issued token.
			if werr := tm.atomicWriteToken(newToken); werr != nil {
				slog.Warn("token refreshed in memory but file persist failed (continuing)",
					"error", werr, "path", tm.tokenPath)
			}

			slog.Info("token refreshed", "expires_at", expiresAt, "attempt", attempt+1)
			return nil
		}

		lastErr = err
		slog.Warn("token refresh attempt failed",
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

	return fmt.Errorf("token refresh failed after %d attempts: %w", maxAttempts, lastErr)
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
