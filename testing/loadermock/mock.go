// Package loadermock provides a programmable in-memory loader sidecar for
// integration tests on both the platform and kit-author side.
//
// Typical use:
//
//	srv := loadermock.NewServer(t)
//	srv.OnLoad(func(req *loader.LoadRequest) (*loader.LoadResponse, error) {
//	    return &loader.LoadResponse{
//	        Status: loader.StatusCompleted,
//	        Stats:  &loader.LoadStats{VerticesCreated: 12},
//	    }, nil
//	})
//	client, _ := loader.NewClient(srv.URL())
//	resp, err := client.Load(ctx, req)
//
// The mock implements [loader.LoaderHandler] so it can also be plugged into
// loader.Handler directly when callers need their own *http.Server (e.g.,
// to add custom middleware).
package loadermock

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
)

// Server is a programmable loader sidecar backed by httptest.Server. Each
// instance records the requests it receives so tests can assert on call
// counts and inputs.
type Server struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	loadFn  func(*loader.LoadRequest) (*loader.LoadResponse, error)
	jobFn   func(string) (*loader.LoadResponse, error)
	formats []string
	health  *loader.HealthResponse

	// recorded
	loadReqs []*loader.LoadRequest
	jobReqs  []string
}

// NewServer starts an httptest.Server backed by [loader.Handler] over the
// mock and registers a t.Cleanup hook to shut it down.
func NewServer(t *testing.T) *Server {
	t.Helper()
	m := &Server{
		t:       t,
		formats: []string{"mock-format"},
		health:  &loader.HealthResponse{Status: "ok", Version: "test"},
	}
	m.srv = httptest.NewServer(loader.Handler(m))
	t.Cleanup(m.srv.Close)
	return m
}

// URL returns the base URL clients should use.
func (m *Server) URL() string { return m.srv.URL }

// Close shuts the server down. Called automatically by t.Cleanup; expose it
// for tests that want explicit ordering.
func (m *Server) Close() { m.srv.Close() }

// OnLoad sets the handler invoked for POST /load. Replaces any previously
// registered function. When unset, /load returns a synthetic completed
// response with no stats.
func (m *Server) OnLoad(fn func(*loader.LoadRequest) (*loader.LoadResponse, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadFn = fn
}

// OnJob sets the handler invoked for GET /jobs/{id}. When unset, /jobs/{id}
// returns ErrJobNotFound for any ID.
func (m *Server) OnJob(fn func(string) (*loader.LoadResponse, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobFn = fn
}

// SetFormats overrides the formats list returned by GET /formats.
func (m *Server) SetFormats(formats ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.formats = append(m.formats[:0], formats...)
}

// SetHealth overrides the response from GET /healthz.
func (m *Server) SetHealth(hr *loader.HealthResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health = hr
}

// LoadCallCount returns how many times POST /load has been invoked.
func (m *Server) LoadCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.loadReqs)
}

// LastLoadRequest returns the most recent /load request body, or nil if
// none has been received.
func (m *Server) LastLoadRequest() *loader.LoadRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.loadReqs) == 0 {
		return nil
	}
	return m.loadReqs[len(m.loadReqs)-1]
}

// JobCallCount returns how many times GET /jobs/{id} has been invoked.
func (m *Server) JobCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobReqs)
}

// --- LoaderHandler ---

// Load implements loader.LoaderHandler.
func (m *Server) Load(_ context.Context, req *loader.LoadRequest) (*loader.LoadResponse, error) {
	m.mu.Lock()
	m.loadReqs = append(m.loadReqs, req)
	fn := m.loadFn
	m.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return &loader.LoadResponse{Status: loader.StatusCompleted}, nil
}

// Job implements loader.LoaderHandler.
func (m *Server) Job(_ context.Context, jobID string) (*loader.LoadResponse, error) {
	m.mu.Lock()
	m.jobReqs = append(m.jobReqs, jobID)
	fn := m.jobFn
	m.mu.Unlock()
	if fn != nil {
		return fn(jobID)
	}
	return nil, &loader.ErrJobNotFound{JobID: jobID}
}

// Formats implements loader.LoaderHandler.
func (m *Server) Formats(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.formats))
	copy(out, m.formats)
	return out, nil
}

// Health implements loader.LoaderHandler.
func (m *Server) Health(_ context.Context) (*loader.HealthResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.health == nil {
		return &loader.HealthResponse{Status: "ok"}, nil
	}
	if m.health.Status != "ok" {
		return m.health, nil
	}
	return m.health, nil
}
