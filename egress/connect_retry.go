package egress

// Background Connect retry (OGA-874).
//
// Binding the listener before Connect stopped a transient external outage from
// crash-looping the container, but on its own it left a component that failed
// Connect degraded FOREVER: nothing ever called Connect again. Recovery
// depended on either the component re-probing inside its own Health (which
// every kit author then has to hand-roll correctly, with its own throttle) or
// an operator pressing "Test Connection". The design promised a background
// retry to close that gap and never specified one — this is it.
//
// Deliberately narrow:
//
//   - It runs ONLY when the INITIAL Connect attempt failed. A component whose
//     Connect succeeded is never called again, so the happy path keeps the
//     original "called ONCE at startup" contract exactly (Requirement 18).
//   - It stops at the FIRST success. This is a recovery mechanism, not a
//     liveness poll — steady-state connectivity is what Health reports.
//   - It backs off exponentially with jitter and does not give up. A component
//     with no external system to reach would have failed at boot; one whose
//     system is down should keep trying quietly, at a cadence that cannot
//     become a second load on an already-struggling dependency. That throttle
//     rationale is the same one coreegress.Component.Health applies.
//   - It does NOT touch health. The SDK does not synthesize a health state on
//     a component's behalf; a component's own Connect records its verdict (as
//     both reference implementations do), so a successful retry surfaces
//     through the component's Health with no SDK involvement.
//
// The connector package carries the symmetric implementation; the two are kept
// deliberately independent, as every other paired egress/connector concern in
// this SDK is.

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	// defaultConnectRetryInitialDelay is short enough that a brief blip clears
	// within a poll interval or two of the platform's health monitor.
	defaultConnectRetryInitialDelay = 5 * time.Second
	// defaultConnectRetryMaxDelay caps the backoff. A dependency down for hours
	// is retried a couple of times a minute at worst, which is negligible load
	// and still recovers promptly once it returns.
	defaultConnectRetryMaxDelay = 2 * time.Minute
)

// retryConnect re-attempts impl.Connect until it succeeds or ctx ends.
//
// Runs in its own goroutine for the life of the process; ctx is the component's
// run context, so shutdown cancels it.
func retryConnect(ctx context.Context, cfg *Config, impl Component) {
	initial, max := connectRetryBounds(cfg)
	delay := initial

	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(delay)):
		}

		if err := impl.Connect(ctx); err != nil {
			cfg.Logger.Warn("egress component: connect retry failed; still degraded",
				"attempt", attempt, "next_retry_in", delay.String(), "error", err)
			delay = min(delay*2, max)
			continue
		}
		cfg.Logger.Info("egress component: connect succeeded on retry; no longer degraded",
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

// jitter spreads retries by up to +50% so a fleet of sidecars that lost the
// same dependency at the same instant does not reconnect in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + rand.N(d/2+1) //nolint:gosec // jitter, not a security decision
}
