package connector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// errHandlerBoom is a handler failure used to assert the drop-uncommitted and
// error-reporting paths.
var errHandlerBoom = errors.New("handler boom")

// Async webhook mode (OGA-892 / OGA-891 spec R4).
//
// The default (sync) path is covered by server_test.go and must stay unchanged —
// TestWebhookMode_SyncIsDefault below pins that. These tests cover the async
// path's own guarantees, the ones that are easy to get wrong when the response is
// decoupled from the work:
//
//   - 202 is sent BEFORE the handler runs (that is the whole point)
//   - the connectPending 503 gate still applies, because after a 202 the provider
//     will not retry and a delivery accepted during boot would be lost
//   - a full queue is a 429, never a silent drop
//   - no-empty-commit and drop-uncommitted-on-error hold identically to sync,
//     since the commit moved into the worker

// newAsyncTestServer builds a server in async mode with a started worker, and
// registers the drain on cleanup so a test cannot leak it.
func newAsyncTestServer(t *testing.T, impl SourceConnector, factory WriterFactory, depth int) *server {
	t.Helper()
	s := &server{
		cfg: &Config{
			WriterFactory:     factory,
			PollInterval:      time.Millisecond,
			Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
			WebhookMode:       WebhookAsync,
			WebhookQueueDepth: depth,
		},
		impl: impl,
		sink: errSink{},
	}
	s.bindings = map[string]Binding{}
	for _, b := range impl.Bindings(context.Background()) {
		s.bindings[b.ID] = b
	}
	s.webhookQueue = make(chan webhookJob, depth)
	s.webhookWorkerDone = make(chan struct{})
	go s.webhookWorker(context.Background())
	t.Cleanup(func() {
		close(s.webhookQueue)
		<-s.webhookWorkerDone
	})
	return s
}

func TestWebhookAsync_Acks202AndProcessesInBackground(t *testing.T) {
	fw := &fakeWriter{}
	done := make(chan string, 1)
	// The handler BLOCKS until released, so if the response were produced by the
	// handler this test would time out rather than pass — that is what makes the
	// "202 before the work" assertion real instead of incidental.
	release := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(ctx context.Context, _ Binding, payload []byte, em *Emitter) error {
			<-release
			if err := em.Entities.WriteVertex(ctx, transfer.Vertex{EntityType: "WorkOrder"}); err != nil {
				return err
			}
			done <- string(payload)
			return nil
		},
	}
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil }, 4)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{"id":"1"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, b)
	}

	close(release)
	select {
	case payload := <-done:
		if payload != `{"id":"1"}` {
			t.Errorf("payload = %q", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not run after the 202")
	}
}

func TestWebhookAsync_CommitsAfterHandler(t *testing.T) {
	fw := &fakeWriter{}
	var mu sync.Mutex
	committed := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(ctx context.Context, _ Binding, _ []byte, em *Emitter) error {
			mu.Lock()
			defer mu.Unlock()
			return em.Entities.WriteVertex(ctx, transfer.Vertex{EntityType: "WorkOrder"})
		},
	}
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) {
		return &closeNotifyWriter{Writer: fw, closed: committed}, nil
	}, 4)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("async batch was never committed")
	}
}

// TestWebhookAsync_NoEmptyCommit — R4.7. A handler that emits nothing must not
// produce an artifact, in async mode exactly as in sync.
func TestWebhookAsync_NoEmptyCommit(t *testing.T) {
	fw := &fakeWriter{}
	ran := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error {
			close(ran)
			return nil // emits nothing
		},
	}
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil }, 4)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()

	<-ran
	// Give the worker a moment past the handler to reach any commit it would make.
	time.Sleep(100 * time.Millisecond)
	if fw.closed {
		t.Error("writer was committed for a handler that emitted nothing (empty artifact)")
	}
}

// TestWebhookAsync_HandlerErrorDropsUncommitted — R4.8. The response is already
// 202, so the only correct outcome is a dropped batch: never a partial commit.
func TestWebhookAsync_HandlerErrorDropsUncommitted(t *testing.T) {
	fw := &fakeWriter{}
	ran := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(ctx context.Context, _ Binding, _ []byte, em *Emitter) error {
			// Emit first, so a commit WOULD have happened had the error been ignored.
			if err := em.Entities.WriteVertex(ctx, transfer.Vertex{EntityType: "WorkOrder"}); err != nil {
				return err
			}
			close(ran)
			return errHandlerBoom
		},
	}
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil }, 4)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	// The caller still sees 202 — the failure cannot be reported to it.
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (an async failure is not reportable to the caller)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	<-ran
	time.Sleep(100 * time.Millisecond)
	if fw.closed {
		t.Error("writer was committed despite a handler error (partial batch persisted)")
	}
}

// TestWebhookAsync_FullQueueIs429 — R4.5. Shedding with a retryable status is the
// only honest answer once inline processing was declined; a silent drop would
// lose the delivery behind a success.
func TestWebhookAsync_FullQueueIs429(t *testing.T) {
	block := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error {
			<-block // occupy the worker so the queue can fill
			return nil
		},
	}
	// Depth 1: one job in the worker, one queued, the third must shed.
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil }, 1)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	defer close(block)

	post := func() int {
		resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	// Drive until we observe a 429. The first post is picked up by the worker
	// (which then blocks), the next fills the depth-1 queue, and subsequent ones
	// must shed. Bounded so a regression fails rather than hangs.
	var saw429 bool
	for i := 0; i < 20 && !saw429; i++ {
		if post() == http.StatusTooManyRequests {
			saw429 = true
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !saw429 {
		t.Error("a saturated async queue never returned 429; deliveries may be dropped silently")
	}
}

// TestWebhookAsync_ConnectPendingStill503 — R4.6, and the subtlest of the set.
// The gate MUST be evaluated in the handler, not the worker: after a 202 the
// provider will not retry, so a delivery accepted during the boot window is lost
// rather than redelivered.
func TestWebhookAsync_ConnectPendingStill503(t *testing.T) {
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error {
			t.Error("handler ran while the initial Connect was still pending")
			return nil
		},
	}
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil }, 4)
	s.connectPending.Store(true)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while connect is pending", resp.StatusCode)
	}
}

// TestWebhookMode_SyncIsDefault — R4.3 / property 12. The zero value must remain
// the pre-existing synchronous behaviour: a connector that does not opt in must
// see no change at all, including the handler's error reaching the caller.
func TestWebhookMode_SyncIsDefault(t *testing.T) {
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error {
			return errHandlerBoom
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	if s.webhookQueue != nil {
		t.Fatal("a server built without WebhookAsync must have no queue")
	}
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 500, not 202: in sync mode the handler's failure IS the response, which is
	// what makes the upstream provider retry.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (sync mode reports the handler error)", resp.StatusCode)
	}
}

func TestWebhookMode_Validity(t *testing.T) {
	for _, tc := range []struct {
		mode WebhookMode
		ok   bool
	}{
		{WebhookSync, true},
		{WebhookAsync, true},
		{WebhookMode("sync"), false},   // the zero value is sync; the literal is not a mode
		{WebhookMode("Async"), false},  // case matters
		{WebhookMode("asynch"), false}, // a typo must not fall back to sync silently
	} {
		if got := tc.mode.valid(); got != tc.ok {
			t.Errorf("WebhookMode(%q).valid() = %v, want %v", tc.mode, got, tc.ok)
		}
	}
}

// closeNotifyWriter signals when the batch was committed, so a test can wait for
// the worker instead of sleeping.
type closeNotifyWriter struct {
	transfer.Writer
	closed chan struct{}
	once   sync.Once
}

func (w *closeNotifyWriter) Close(ctx context.Context) (*transfer.Receipt, error) {
	r, err := w.Writer.Close(ctx)
	w.once.Do(func() { close(w.closed) })
	return r, err
}
