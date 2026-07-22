package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

func freshWriterFactory() (WriterFactory, *int32) {
	var commits int32
	f := func(context.Context, Binding) (transfer.Writer, error) {
		return &countingCloseWriter{commits: &commits}, nil
	}
	return f, &commits
}

// countingCloseWriter records how many times Close (commit) was called across
// all writers minted by a factory.
type countingCloseWriter struct {
	commits *int32
	wrote   bool
}

func (c *countingCloseWriter) WriteVertex(context.Context, transfer.Vertex) error {
	c.wrote = true
	return nil
}
func (c *countingCloseWriter) WriteEdge(context.Context, transfer.Edge) error {
	c.wrote = true
	return nil
}
func (c *countingCloseWriter) WriteEntityType(context.Context, transfer.EntityTypeDef) error {
	c.wrote = true
	return nil
}
func (c *countingCloseWriter) WriteHierarchy(context.Context, transfer.HierarchyEntry) error {
	c.wrote = true
	return nil
}
func (c *countingCloseWriter) Close(context.Context) (*transfer.Receipt, error) {
	atomic.AddInt32(c.commits, 1)
	return &transfer.Receipt{JobID: "job"}, nil
}

// --- empty-batch suppression (no empty commits) ---

func TestRunSync_EmptyEmitSkipsCommit(t *testing.T) {
	fw := &fakeWriter{}
	fc := &fakeConnector{
		bindings: []Binding{{ID: "b"}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			return &SyncResult{NextCursor: "c1"}, nil // emits nothing
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil })

	res, err := s.runSync(t.Context(), fc.bindings[0], "")
	if err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if res.NextCursor != "c1" {
		t.Errorf("cursor = %q, want c1", res.NextCursor)
	}
	if fw.closed {
		t.Error("an empty Sync must NOT commit (no empty artifact)")
	}
}

func TestWebhook_EmptyEmitNoCommit(t *testing.T) {
	fw := &fakeWriter{}
	fc := &fakeConnector{
		bindings: []Binding{{ID: "b", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error {
			return nil // emits nothing
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return fw, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook/b", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fw.closed {
		t.Error("a no-op webhook must NOT commit an empty artifact")
	}
}

// --- drain pagination + runaway guard ---

func TestDrain_MultiPageAdvancing(t *testing.T) {
	var calls int32
	factory, commits := freshWriterFactory()
	fc := &fakeConnector{
		bindings: []Binding{{ID: "b"}},
		syncFn: func(_ context.Context, _ Binding, _ string, em *Emitter) (*SyncResult, error) {
			_ = em.Entities.WriteVertex(context.Background(), transfer.Vertex{EntityType: "X"})
			switch atomic.AddInt32(&calls, 1) {
			case 1:
				return &SyncResult{NextCursor: "c1", HasMore: true}, nil
			case 2:
				return &SyncResult{NextCursor: "c2", HasMore: true}, nil
			default:
				return &SyncResult{NextCursor: "c3", HasMore: false}, nil
			}
		},
	}
	s := newTestServer(fc, factory)
	final := s.drain(t.Context(), fc.bindings[0], "")
	if final != "c3" {
		t.Errorf("final cursor = %q, want c3", final)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (drained all pages)", calls)
	}
	if *commits != 3 {
		t.Errorf("commits = %d, want 3 (each non-empty page committed)", *commits)
	}
}

func TestDrain_NoAdvanceHasMoreStops(t *testing.T) {
	var calls int32
	factory, _ := freshWriterFactory()
	fc := &fakeConnector{
		bindings: []Binding{{ID: "b"}},
		syncFn: func(_ context.Context, _ Binding, _ string, em *Emitter) (*SyncResult, error) {
			atomic.AddInt32(&calls, 1)
			_ = em.Entities.WriteVertex(context.Background(), transfer.Vertex{EntityType: "X"})
			return &SyncResult{NextCursor: "", HasMore: true}, nil // HasMore but never advances
		},
	}
	s := newTestServer(fc, factory)
	final := s.drain(t.Context(), fc.bindings[0], "")
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (runaway guard must stop a no-advance HasMore loop)", calls)
	}
	if final != "" {
		t.Errorf("final cursor = %q, want empty", final)
	}
}

func TestDrain_StopsOnError(t *testing.T) {
	var calls int32
	factory, _ := freshWriterFactory()
	fc := &fakeConnector{
		bindings: []Binding{{ID: "b"}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			atomic.AddInt32(&calls, 1)
			return nil, errors.New("source down")
		},
	}
	s := newTestServer(fc, factory)
	final := s.drain(t.Context(), fc.bindings[0], "c-prev")
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if final != "c-prev" {
		t.Errorf("cursor must stay %q on error, got %q", "c-prev", final)
	}
}

// --- oversized webhook body ---

func TestWebhook_OversizedBody413(t *testing.T) {
	fc := &fakeConnector{
		bindings:  []Binding{{ID: "b", Mode: ModeWebhook}},
		webhookFn: func(context.Context, Binding, []byte, *Emitter) error { return nil },
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	big := strings.NewReader(strings.Repeat("a", (8<<20)+1024)) // > 8 MiB
	resp, err := http.Post(srv.URL+"/webhook/b", "application/octet-stream", big)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (oversized body must be rejected, not truncated)", resp.StatusCode)
	}
}

// --- startup binding validation ---

func TestValidateBindings(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		m, err := validateBindings([]Binding{
			{ID: "a", Mode: ModePoll}, {ID: "b", Mode: ModeWebhook}, {ID: "c", Mode: ModeBoth}, {ID: "d"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(m) != 4 {
			t.Errorf("map size = %d, want 4", len(m))
		}
	})
	t.Run("empty ID", func(t *testing.T) {
		if _, err := validateBindings([]Binding{{ID: ""}}); err == nil {
			t.Error("empty binding ID must be rejected")
		}
	})
	t.Run("duplicate ID", func(t *testing.T) {
		_, err := validateBindings([]Binding{{ID: "a"}, {ID: "a"}})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("duplicate binding ID must be rejected, got %v", err)
		}
	})
	t.Run("invalid mode", func(t *testing.T) {
		_, err := validateBindings([]Binding{{ID: "a", Mode: IngressMode("stream")}})
		if err == nil || !strings.Contains(err.Error(), "invalid mode") {
			t.Errorf("invalid mode must be rejected, got %v", err)
		}
	})
}

// --- webhook validation handshake ---

type validatingConnector struct {
	*fakeConnector
}

func (v *validatingConnector) ValidateWebhook(_ context.Context, _ Binding, query map[string][]string) ([]byte, error) {
	if c := query["challenge"]; len(c) > 0 {
		return []byte(c[0]), nil
	}
	return nil, nil
}

func TestWebhookValidate_EchoesChallenge(t *testing.T) {
	fc := &validatingConnector{&fakeConnector{bindings: []Binding{{ID: "b", Mode: ModeWebhook}}}}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/webhook/b?challenge=xyz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := make([]byte, 8)
	n, _ := resp.Body.Read(body)
	if string(body[:n]) != "xyz" {
		t.Errorf("validation echo = %q, want xyz", string(body[:n]))
	}
}

func TestWebhookValidate_NoHandlerReturns200(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "b", Mode: ModeWebhook}}}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return &fakeWriter{}, nil })
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/webhook/b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
