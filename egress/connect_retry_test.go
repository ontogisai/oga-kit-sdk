package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// flakyComponent fails Connect the first failUntil times, then succeeds.
type flakyComponent struct {
	stubComponent
	failUntil int32
	attempts  atomic.Int32
	succeeded atomic.Bool
}

func (f *flakyComponent) Connect(context.Context) error {
	n := f.attempts.Add(1)
	if n <= f.failUntil {
		return errors.New("external system unreachable")
	}
	f.succeeded.Store(true)
	return nil
}

func quietRetryConfig() *Config {
	cfg := &Config{
		Port:                     "0",
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConnectRetryInitialDelay: time.Millisecond,
		ConnectRetryMaxDelay:     2 * time.Millisecond,
	}
	cfg.defaults()
	return cfg
}

// The core property: a component that failed its initial Connect recovers on
// its own, with no operator action and no kit-side re-probe loop.
func TestRetryConnect_RecoversWithoutOperatorAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	impl := &flakyComponent{failUntil: 3}
	done := make(chan struct{})
	go func() { retryConnect(ctx, quietRetryConfig(), impl); close(done) }()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("retryConnect did not return; it must stop at the first success")
	}
	if !impl.succeeded.Load() {
		t.Error("Connect never succeeded")
	}
	if got := impl.attempts.Load(); got != 4 {
		t.Errorf("attempts = %d, want 4 (3 failures then success)", got)
	}
}

// It must STOP at the first success — this is a recovery mechanism, not a
// liveness poll. Continuing would re-Connect a healthy component forever.
func TestRetryConnect_StopsAtFirstSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	impl := &flakyComponent{failUntil: 0} // succeeds immediately
	done := make(chan struct{})
	go func() { retryConnect(ctx, quietRetryConfig(), impl); close(done) }()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("retryConnect did not return on immediate success")
	}
	// Give a stray loop a chance to make another call before asserting.
	time.Sleep(20 * time.Millisecond)
	if got := impl.attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 — it must not keep polling a healthy component", got)
	}
}

// Cancellation must end the loop promptly, so shutdown is not delayed by a
// pending backoff.
func TestRetryConnect_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	impl := &flakyComponent{failUntil: 1 << 30} // never succeeds

	cfg := quietRetryConfig()
	cfg.ConnectRetryInitialDelay = 50 * time.Millisecond
	cfg.ConnectRetryMaxDelay = 50 * time.Millisecond

	done := make(chan struct{})
	go func() { retryConnect(ctx, cfg, impl); close(done) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retryConnect did not stop on context cancel")
	}
}

// A SUCCESSFUL initial Connect must never schedule a retry — this is what keeps
// the happy path identical to the original "called ONCE at startup" contract
// (Requirement 18).
func TestListenAndServe_NoRetryWhenInitialConnectSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	impl := &flakyComponent{failUntil: 0}
	cfg := &Config{
		Port:                     "0",
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConnectRetryInitialDelay: time.Millisecond,
		ConnectRetryMaxDelay:     time.Millisecond,
	}

	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, impl) }()
	time.Sleep(100 * time.Millisecond) // ample time for many retries, had any been scheduled
	cancel()
	<-done

	if got := impl.attempts.Load(); got != 1 {
		t.Errorf("Connect called %d times, want exactly 1 — a successful initial "+
			"Connect must not start the retry loop", got)
	}
}

// DisableConnectRetry means the component owns recovery.
func TestListenAndServe_DisableConnectRetryHonored(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	impl := &flakyComponent{failUntil: 1 << 30}
	cfg := &Config{
		Port:                     "0",
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConnectRetryInitialDelay: time.Millisecond,
		ConnectRetryMaxDelay:     time.Millisecond,
		DisableConnectRetry:      true,
	}

	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, impl) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if got := impl.attempts.Load(); got != 1 {
		t.Errorf("Connect called %d times, want exactly 1 when retry is disabled", got)
	}
}

// A FAILED initial Connect DOES start the retry loop.
func TestListenAndServe_FailedInitialConnectStartsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	impl := &flakyComponent{failUntil: 2}
	cfg := &Config{
		Port:                     "0",
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConnectRetryInitialDelay: time.Millisecond,
		ConnectRetryMaxDelay:     2 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, impl) }()

	deadline := time.After(3 * time.Second)
	for !impl.succeeded.Load() {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("Connect never succeeded on retry (attempts=%d)", impl.attempts.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestConnectRetryBounds(t *testing.T) {
	cases := []struct {
		name                 string
		initial, max         time.Duration
		wantInitial, wantMax time.Duration
	}{
		{"zero applies defaults", 0, 0, defaultConnectRetryInitialDelay, defaultConnectRetryMaxDelay},
		{"explicit values honored", time.Second, time.Minute, time.Second, time.Minute},
		{"max below initial is clamped up", 10 * time.Second, time.Second, 10 * time.Second, 10 * time.Second},
		{"negative applies defaults", -1, -1, defaultConnectRetryInitialDelay, defaultConnectRetryMaxDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotInitial, gotMax := connectRetryBounds(&Config{
				ConnectRetryInitialDelay: tc.initial,
				ConnectRetryMaxDelay:     tc.max,
			})
			if gotInitial != tc.wantInitial || gotMax != tc.wantMax {
				t.Errorf("bounds = (%v, %v), want (%v, %v)",
					gotInitial, gotMax, tc.wantInitial, tc.wantMax)
			}
		})
	}
}

func TestJitter_WithinBounds(t *testing.T) {
	const d = 100 * time.Millisecond
	for range 200 {
		got := jitter(d)
		if got < d || got > d+d/2+1 {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, d, d+d/2+1)
		}
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
}
