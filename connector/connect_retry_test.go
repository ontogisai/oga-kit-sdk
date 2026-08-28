package connector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// flakyConnector fails Connect the first failUntil times, then succeeds.
type flakyConnector struct {
	*fakeConnector
	failUntil int32
	attempts  atomic.Int32
	succeeded atomic.Bool
}

func (f *flakyConnector) Connect(context.Context) error {
	n := f.attempts.Add(1)
	if n <= f.failUntil {
		return errors.New("upstream unreachable")
	}
	f.succeeded.Store(true)
	return nil
}

func newFlaky(failUntil int32) *flakyConnector {
	return &flakyConnector{
		fakeConnector: &fakeConnector{
			bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
			health:   map[string]Health{"wo": {OK: false}},
		},
		failUntil: failUntil,
	}
}

func quietRetryConfig() *Config {
	cfg := &Config{
		Port:                     "0",
		WriterFactory:            func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil },
		PollInterval:             time.Hour,
		Logger:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConnectRetryInitialDelay: time.Millisecond,
		ConnectRetryMaxDelay:     2 * time.Millisecond,
	}
	cfg.defaults()
	return cfg
}

// The core property: a connector that failed its initial Connect recovers on its
// own. This matters more than for egress because the poll loops keep running —
// without it a connector that never established credentials polls fruitlessly
// forever.
func TestRetryConnect_RecoversWithoutOperatorAction(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	impl := newFlaky(3)
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

func TestRetryConnect_StopsAtFirstSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	impl := newFlaky(0)
	done := make(chan struct{})
	go func() { retryConnect(ctx, quietRetryConfig(), impl); close(done) }()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("retryConnect did not return on immediate success")
	}
	time.Sleep(20 * time.Millisecond)
	if got := impl.attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 — it must not keep polling a healthy connector", got)
	}
}

func TestRetryConnect_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	impl := newFlaky(1 << 30)

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

// A successful initial Connect must not schedule a retry (Requirement 18).
func TestListenAndServe_NoRetryWhenInitialConnectSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	impl := newFlaky(0)
	cfg := quietRetryConfig()

	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, impl) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if got := impl.attempts.Load(); got != 1 {
		t.Errorf("Connect called %d times, want exactly 1 — a successful initial "+
			"Connect must not start the retry loop", got)
	}
}

func TestListenAndServe_DisableConnectRetryHonored(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	impl := newFlaky(1 << 30)
	cfg := quietRetryConfig()
	cfg.DisableConnectRetry = true

	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, impl) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if got := impl.attempts.Load(); got != 1 {
		t.Errorf("Connect called %d times, want exactly 1 when retry is disabled", got)
	}
}

func TestListenAndServe_FailedInitialConnectStartsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	impl := newFlaky(2)
	cfg := quietRetryConfig()

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
