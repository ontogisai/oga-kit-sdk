package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// --- fakes ---

type fakeWriter struct {
	vertices []transfer.Vertex
	closed   bool
	closeErr error
}

func (f *fakeWriter) WriteVertex(_ context.Context, v transfer.Vertex) error {
	f.vertices = append(f.vertices, v)
	return nil
}
func (f *fakeWriter) WriteEdge(context.Context, transfer.Edge) error                { return nil }
func (f *fakeWriter) WriteEntityType(context.Context, transfer.EntityTypeDef) error { return nil }
func (f *fakeWriter) WriteRelationshipType(context.Context, transfer.RelationshipTypeDef) error {
	return nil
}
func (f *fakeWriter) WriteHierarchy(context.Context, transfer.HierarchyEntry) error { return nil }
func (f *fakeWriter) Close(context.Context) (*transfer.Receipt, error) {
	f.closed = true
	return &transfer.Receipt{JobID: "job-1"}, f.closeErr
}

type fakeConnector struct {
	bindings   []Binding
	syncFn     func(ctx context.Context, b Binding, cursor string, em *Emitter) (*SyncResult, error)
	webhookFn  func(ctx context.Context, b Binding, payload []byte, em *Emitter) error
	health     map[string]Health
	connectErr error
}

func (f *fakeConnector) Bindings(context.Context) []Binding { return f.bindings }
func (f *fakeConnector) Connect(context.Context) error      { return f.connectErr }
func (f *fakeConnector) Sync(ctx context.Context, b Binding, cursor string, em *Emitter) (*SyncResult, error) {
	return f.syncFn(ctx, b, cursor, em)
}
func (f *fakeConnector) HandleWebhook(ctx context.Context, b Binding, payload []byte, em *Emitter) error {
	return f.webhookFn(ctx, b, payload, em)
}
func (f *fakeConnector) Health(context.Context) map[string]Health { return f.health }

func newTestServer(impl SourceConnector, factory WriterFactory) *server {
	s := &server{
		cfg: &Config{
			WriterFactory: factory,
			PollInterval:  time.Millisecond,
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		impl: impl,
		sink: errSink{},
	}
	s.bindings = map[string]Binding{}
	for _, b := range impl.Bindings(context.Background()) {
		s.bindings[b.ID] = b
	}
	return s
}

// --- runSync ---

func TestRunSync_SuccessCommitsBatch(t *testing.T) {
	fw := &fakeWriter{}
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", ExternalSystem: "wo_mgmt", SourceType: "wo_status"}},
		syncFn: func(_ context.Context, b Binding, _ string, em *Emitter) (*SyncResult, error) {
			_ = em.Entities.WriteVertex(context.Background(), transfer.Vertex{
				EntityType:     "WorkOrder",
				CorrelationKey: &transfer.CorrelationKey{ExternalSystem: b.ExternalSystem, ExternalRecordID: "WO-1"},
			})
			return &SyncResult{NextCursor: "c2", Emitted: 1}, nil
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil })

	res, err := s.runSync(t.Context(), fc.bindings[0], "c1")
	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if res.NextCursor != "c2" {
		t.Errorf("cursor = %q, want c2", res.NextCursor)
	}
	if !fw.closed {
		t.Error("writer must be committed (closed) on success")
	}
	if len(fw.vertices) != 1 || fw.vertices[0].CorrelationKey == nil {
		t.Errorf("expected one vertex with CorrelationKey, got %+v", fw.vertices)
	}
}

func TestRunSync_ErrorDropsBatch(t *testing.T) {
	fw := &fakeWriter{}
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo"}},
		syncFn: func(_ context.Context, _ Binding, _ string, em *Emitter) (*SyncResult, error) {
			_ = em.Entities.WriteVertex(context.Background(), transfer.Vertex{EntityType: "WorkOrder"})
			return nil, errors.New("source unreachable")
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil })

	if _, err := s.runSync(t.Context(), fc.bindings[0], "c1"); err == nil {
		t.Fatal("expected error")
	}
	if fw.closed {
		t.Error("writer must NOT be committed when Sync errors (partial batch dropped)")
	}
}

// --- webhook ---

func TestWebhook_RoutesAndCommits(t *testing.T) {
	fw := &fakeWriter{}
	var gotPayload string
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", SourceType: "wo_status", Mode: ModeWebhook}},
		webhookFn: func(_ context.Context, _ Binding, payload []byte, em *Emitter) error {
			gotPayload = string(payload)
			return em.Entities.WriteVertex(context.Background(), transfer.Vertex{EntityType: "WorkOrder"})
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{"status":"done"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if gotPayload != `{"status":"done"}` {
		t.Errorf("payload = %q", gotPayload)
	}
	if !fw.closed {
		t.Error("writer must be committed on webhook success")
	}
}

func TestWebhook_UnknownBinding404(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "wo", Mode: ModeWebhook}}}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/nope", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestWebhook_PollOnlyBindingRejected(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "wo", Mode: ModePoll}}}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("poll-only binding should reject webhook, status = %d", resp.StatusCode)
	}
}

// --- health ---

func TestHealth(t *testing.T) {
	cases := []struct {
		name   string
		health map[string]Health
		want   int
	}{
		{"all ok", map[string]Health{"wo": {OK: true}}, http.StatusOK},
		{"one down", map[string]Health{"wo": {OK: false, Message: "auth failed"}}, http.StatusServiceUnavailable},
		{"missing binding", map[string]Health{}, http.StatusServiceUnavailable},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeConnector{bindings: []Binding{{ID: "wo"}}, health: tt.health}
			s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
			srv := httptest.NewServer(s.mux())
			defer srv.Close()
			resp, err := http.Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

// --- OGA-874: bind-before-connect, /livez, /connector/test-connection ---

// A Connect failure must NOT abort ListenAndServe — the HTTP server
// (including /livez and /healthz) must still come up, so the container is
// never crash-looped over a transient third-party outage. Property P1/P5.
func TestListenAndServe_ConnectFailureDoesNotAbortStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fc := &fakeConnector{
		bindings:   []Binding{{ID: "wo", Mode: ModeWebhook}}, // webhook-only: no poll loop to race against Connect
		connectErr: errors.New("wo mgmt unreachable"),
		health:     map[string]Health{"wo": {OK: false, Message: "not connected"}},
	}
	cfg := &Config{
		Port:          "0",
		WriterFactory: func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil },
		PollInterval:  time.Hour, // avoid poll noise during the test window
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, fc) }()

	select {
	case err := <-done:
		t.Fatalf("ListenAndServe returned early (err=%v); a Connect failure must not abort startup", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("ListenAndServe error after cancel = %v, want nil", err)
	}
}

// /livez never depends on Connect, Bindings health, or Health(ctx).
func TestLivez_AlwaysOKRegardlessOfHealth(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "wo"}}, health: map[string]Health{"wo": {OK: false}}}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + PathLivez)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 even when Health reports unhealthy", PathLivez, resp.StatusCode)
	}
}

// probingConnector implements Prober alongside fakeConnector so
// TestConnection can be exercised independent of Health.
type probingConnector struct {
	*fakeConnector
	probe func(ctx context.Context) map[string]Health
}

func (p *probingConnector) TestConnection(ctx context.Context) map[string]Health {
	return p.probe(ctx)
}

var _ Prober = (*probingConnector)(nil)

func TestTestConnection_UsesProberWhenImplemented(t *testing.T) {
	called := false
	fc := &probingConnector{
		fakeConnector: &fakeConnector{
			bindings: []Binding{{ID: "wo"}},
			health:   map[string]Health{"wo": {OK: false, Message: "stale cache"}},
		},
		probe: func(context.Context) map[string]Health {
			called = true
			return map[string]Health{"wo": {OK: true, Message: "fresh probe succeeded"}}
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+PathTestConnection, "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !called {
		t.Fatal("TestConnection was not invoked; the server fell back to Health despite Prober being implemented")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]Health
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got["wo"].OK || got["wo"].Message != "fresh probe succeeded" {
		t.Errorf("body = %+v, want the fresh probe result, not the cached Health", got)
	}
}

func TestTestConnection_FallsBackToHealthWhenNoProber(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "wo"}}, health: map[string]Health{"wo": {OK: false, Message: "auth expired"}}}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+PathTestConnection, "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var got map[string]Health
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["wo"].OK || got["wo"].Message != "auth expired" {
		t.Errorf("body = %+v, want the connector's plain Health() result", got)
	}
}

// --- OGA-874 follow-up: webhook DELIVERY is gated until the initial Connect
// attempt completes; the VALIDATION handshake deliberately is not.

type blockingConnectConnector struct {
	*fakeConnector
	release    chan struct{}
	webhookHit atomic.Bool
}

func (b *blockingConnectConnector) Connect(context.Context) error {
	<-b.release
	return nil
}

func TestHandleWebhook_Gated503UntilInitialConnectCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	inner := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		health:   map[string]Health{"wo": {OK: true}},
	}
	impl := &blockingConnectConnector{fakeConnector: inner, release: make(chan struct{})}
	inner.webhookFn = func(context.Context, Binding, []byte, *Emitter) error {
		impl.webhookHit.Store(true)
		return nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	cfg := &Config{
		Port:          port,
		WriterFactory: func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil },
		PollInterval:  time.Hour,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, impl) }()

	base := "http://127.0.0.1:" + port
	var up bool
	for range 100 {
		resp, gerr := http.Get(base + PathLivez)
		if gerr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				up = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !up {
		t.Fatal("/livez never answered 200; the listener must bind before Connect")
	}

	resp, err := http.Post(base+"/webhook/wo", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	code := resp.StatusCode
	_ = resp.Body.Close()
	if code != http.StatusServiceUnavailable {
		t.Errorf("webhook delivery during in-flight Connect = %d, want 503", code)
	}
	if impl.webhookHit.Load() {
		t.Error("HandleWebhook was called before the initial Connect attempt completed")
	}

	close(impl.release)
	var opened bool
	for range 100 {
		r2, perr := http.Post(base+"/webhook/wo", "application/json", strings.NewReader(`{"x":1}`))
		if perr == nil {
			c := r2.StatusCode
			_ = r2.Body.Close()
			if c != http.StatusServiceUnavailable {
				opened = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !opened {
		t.Error("webhook delivery still refused after Connect completed; the gate must clear")
	}

	cancel()
	<-done
}

// A directly-constructed server has no Connect lifecycle, so delivery must not
// be gated — keeps every pre-existing webhook test valid.
func TestHandleWebhook_DirectlyConstructedServerIsNotGated(t *testing.T) {
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error {
			return nil
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Error("a directly-constructed server must not gate webhook delivery")
	}
}
