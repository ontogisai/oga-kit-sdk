package connector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
func (f *fakeWriter) WriteHierarchy(context.Context, transfer.HierarchyEntry) error { return nil }
func (f *fakeWriter) Close(context.Context) (*transfer.Receipt, error) {
	f.closed = true
	return &transfer.Receipt{JobID: "job-1"}, f.closeErr
}

type fakeConnector struct {
	bindings  []Binding
	syncFn    func(ctx context.Context, b Binding, cursor string, em *Emitter) (*SyncResult, error)
	webhookFn func(ctx context.Context, b Binding, payload []byte, em *Emitter) error
	health    map[string]Health
}

func (f *fakeConnector) Bindings(context.Context) []Binding { return f.bindings }
func (f *fakeConnector) Connect(context.Context) error      { return nil }
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
