package connector

// Background Connect retry (OGA-874). Symmetric with egress/connect_retry.go —
// see that file for the full rationale; the short version:
//
// Binding the listener before Connect stopped a transient external outage from
// crash-looping the container, but on its own it left a connector that failed
// Connect degraded FOREVER, because nothing ever called Connect again. Recovery
// depended on the connector re-probing inside its own Health (hand-rolled per
// kit) or an operator pressing "Test Connection".
//
// Narrow by design: runs ONLY when the INITIAL attempt failed (so a connector
// whose Connect succeeded is never called again, preserving the original
// "called once" contract), stops at the first success, backs off exponentially
// with jitter so it cannot become a second load on a struggling upstream, and
// never synthesizes health — the connector's own Connect records its verdict.
//
// A connector matters slightly more than an egress component here: its poll
// loops keep running through the outage and will keep failing Sync, so without
// a Connect retry a connector whose credentials were never established would
// poll fruitlessly forever.

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	defaultConnectRetryInitialDelay = 5 * time.Second
	defaultConnectRetryMaxDelay     = 2 * time.Minute
)

// retryConnect re-attempts impl.Connect until it succeeds or ctx ends.
func retryConnect(ctx context.Context, cfg *Config, impl SourceConnector) {
	initial, max := connectRetryBounds(cfg)
	delay := initial

	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(delay)):
		}

		if err := impl.Connect(ctx); err != nil {
			cfg.Logger.Warn("source connector: connect retry failed; still degraded",
				"attempt", attempt, "next_retry_in", delay.String(), "error", err)
			delay = min(delay*2, max)
			continue
		}
		cfg.Logger.Info("source connector: connect succeeded on retry; no longer degraded",
			"attempt", attempt)
		return
	}
}

// connectRetryBounds resolves the configured backoff bounds, applying defaults
// and keeping max >= initial so a misconfiguration cannot invert the backoff.
func connectRetryBounds(cfg *Config) (initial, max time.Duration) {
	initial = cfg.ConnectRetryInitialDelay
	if initial <= 0 {
		initial = defaultConnectRetryInitialDelay
	}
	max = cfg.ConnectRetryMaxDelay
	if max <= 0 {
		max = defaultConnectRetryMaxDelay
	}
	if max < initial {
		max = initial
	}
	return initial, max
}

// jitter spreads retries by up to +50% so a fleet of connectors that lost the
// same upstream at the same instant does not reconnect in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + rand.N(d/2+1) //nolint:gosec // jitter, not a security decision
}
