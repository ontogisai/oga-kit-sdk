package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ontogisai/oga-kit-sdk/internal/redact"
	"github.com/ontogisai/oga-kit-sdk/kitlog"
)

// Mint retry: each mint is tried up to mintAttempts times, waiting
// mintFirstDelay, then twice as long each time (1, 2, 4 and 8 s), between
// attempts. mintFirstDelay is a variable so tests can shorten it.
const mintAttempts = 5

var mintFirstDelay = 1 * time.Second

// TokenManager holds a sidecar's platform workload token. It mints the token
// from the sidecar's bootstrap identity (see [BootstrapProvider]) at
// construction and again at 50% of each token's lifetime, retrying a failed
// mint with exponential backoff. The token lives in memory only; nothing is
// read from or written to disk.
type TokenManager struct {
	client *http.Client

	bootstrap  BootstrapProvider
	issueURL   string
	agentRegID string

	mu        sync.RWMutex
	current   string
	expiresAt time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// TokenManagerConfig configures the token manager. Every field is required.
type TokenManagerConfig struct {
	// Bootstrap is the sidecar's bootstrap identity, presented to the mint
	// endpoint on every mint. It is read each time, so a rotated credential
	// (a kubelet-refreshed ServiceAccount token) is picked up.
	Bootstrap BootstrapProvider

	// IssueURL is the gateway mint endpoint:
	// PLATFORM_GATEWAY_URL + "/auth/token/issue".
	IssueURL string

	// AgentRegistrationID is the "{tenant}.{name}" identity the mint endpoint
	// authorizes the bootstrap credential against (AGENT_REGISTRATION_ID).
	AgentRegistrationID string
}

// NewTokenManager mints the first workload token and starts the renewal
// goroutine. A failed first mint is returned as an error: the sidecar cannot
// prove its identity and must not start serving. Call Stop on shutdown.
//
// Renewal runs until Stop is called or ctx ends, whichever comes first. Pass a
// context that lives as long as the sidecar: with a shorter one (a startup
// timeout, for example) the token is never renewed and the gateway answers 401
// once it expires.
func NewTokenManager(ctx context.Context, cfg *TokenManagerConfig) (*TokenManager, error) {
	if cfg == nil {
		return nil, errors.New("token manager config is required")
	}
	if cfg.Bootstrap == nil {
		return nil, errors.New("bootstrap identity is required")
	}
	if cfg.IssueURL == "" {
		return nil, errors.New("IssueURL is required")
	}
	if cfg.AgentRegistrationID == "" {
		return nil, errors.New("AgentRegistrationID is required")
	}
	tm := &TokenManager{
		client:     &http.Client{Timeout: 30 * time.Second},
		bootstrap:  cfg.Bootstrap,
		issueURL:   cfg.IssueURL,
		agentRegID: cfg.AgentRegistrationID,
		stopCh:     make(chan struct{}),
	}

	if err := tm.mint(ctx); err != nil {
		return nil, fmt.Errorf("initial workload token mint: %w", err)
	}

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

// Stop stops the renewal goroutine and waits for it to return. It is safe to
// call more than once.
func (tm *TokenManager) Stop() {
	tm.stopOnce.Do(func() { close(tm.stopCh) })
	tm.wg.Wait()
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
			if err := tm.mint(ctx); err != nil {
				kitlog.From(ctx).Error("token renewal failed", kitlog.Err(err))
			}
		case <-tm.stopCh:
			return
		case <-ctx.Done():
			// Logged, because a context that ends before the sidecar does leaves
			// the token unrenewed until it expires.
			kitlog.From(ctx).Info("token renewal stopped: context done", "expires_at", expiresAt)
			return
		}
	}
}

// mint obtains a fresh workload token from the gateway mint endpoint using the
// bootstrap identity, trying up to mintAttempts times with exponential backoff
// between attempts. The gateway never needs the prior workload token (the
// bootstrap identity is authoritative), so an expired token cannot block
// renewal. On success the token replaces the current one in memory.
func (tm *TokenManager) mint(ctx context.Context) error {
	delay := mintFirstDelay

	var lastErr error
	for attempt := range mintAttempts {
		newToken, expiresAt, err := tm.doMint(ctx)
		if err == nil {
			tm.mu.Lock()
			tm.current = newToken
			tm.expiresAt = expiresAt
			tm.mu.Unlock()

			kitlog.From(ctx).Info("token minted", "expires_at", expiresAt, "attempt", attempt+1)
			return nil
		}

		lastErr = err
		if attempt == mintAttempts-1 {
			break
		}
		kitlog.From(ctx).Warn("token mint attempt failed",
			"attempt", attempt+1,
			"max_attempts", mintAttempts,
			kitlog.Err(err),
			"retry_in", delay,
		)

		select {
		case <-time.After(delay):
			delay *= 2
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("token mint failed after %d attempts: %w", mintAttempts, lastErr)
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

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("mint returned %d: %s", resp.StatusCode, errorBodySummary(resp.Body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read mint response: %w", err)
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

// errorBodySummary reads an error response up to 64 KiB and returns at most
// redact.MaxQuotedBody bytes of it for an error message (OGA-1036). The
// gateway's answer is its own error JSON; anything between the sidecar and the
// gateway can answer with more.
func errorBodySummary(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	return redact.ErrorBody(b)
}
