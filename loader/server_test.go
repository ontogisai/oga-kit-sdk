package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/loader"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// stubHandler is a minimal LoaderHandler used to drive server tests.
type stubHandler struct {
	loadFn    func(context.Context, *loader.LoadRequest) (*loader.LoadResponse, error)
	jobFn     func(context.Context, string) (*loader.LoadResponse, error)
	formatsFn func(context.Context) ([]string, error)
	healthFn  func(context.Context) (*loader.HealthResponse, error)
	loadCalls atomic.Int64
}

func (s *stubHandler) Load(ctx context.Context, lc *loader.LoadContext) (*loader.LoadResponse, error) {
	s.loadCalls.Add(1)
	if s.loadFn != nil {
		return s.loadFn(ctx, lc.Request)
	}
	return &loader.LoadResponse{
		Status:      loader.StatusCompleted,
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
		Stats:       &loader.LoadStats{VerticesCreated: 1},
	}, nil
}
func (s *stubHandler) Job(ctx context.Context, id string) (*loader.LoadResponse, error) {
	if s.jobFn != nil {
		return s.jobFn(ctx, id)
	}
	return nil, &loader.ErrJobNotFound{JobID: id}
}
func (s *stubHandler) Formats(ctx context.Context) ([]string, error) {
	if s.formatsFn != nil {
		return s.formatsFn(ctx)
	}
	return []string{"test-format"}, nil
}
func (s *stubHandler) Health(ctx context.Context) (*loader.HealthResponse, error) {
	if s.healthFn != nil {
		return s.healthFn(ctx)
	}
	return &loader.HealthResponse{Status: "ok"}, nil
}

func newTestClient(t *testing.T, h loader.LoaderHandler) (*loader.Client, *httptest.Server) {
	t.Helper()
	// Default tests get a no-op writer factory so the handler can run
	// without standing up a real platform commit client. Tests that
	// want to exercise transfer behavior use newTestClientWithWriter.
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(h, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)
	c, err := loader.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, srv
}

func TestLoad_Sync_Success(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{}
	client, _ := newTestClient(t, stub)

	resp, err := client.Load(context.Background(), &loader.LoadRequest{
		TenantID:  "tenant-001",
		KitID:     "test-kit",
		SourceURI: "file:///data/x.json",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if resp.Status != loader.StatusCompleted {
		t.Errorf("status = %q, want completed", resp.Status)
	}
	if resp.Stats == nil || resp.Stats.VerticesCreated != 1 {
		t.Errorf("stats = %+v, want vertices_created=1", resp.Stats)
	}
	if stub.loadCalls.Load() != 1 {
		t.Errorf("load calls = %d, want 1", stub.loadCalls.Load())
	}
}

func TestLoad_Async_Returns202(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{
		loadFn: func(_ context.Context, _ *loader.LoadRequest) (*loader.LoadResponse, error) {
			return &loader.LoadResponse{
				JobID:  "job-123",
				Status: loader.StatusRunning,
			}, nil
		},
	}
	// Test the raw HTTP response directly to verify the 202 status code,
	// since the client treats both 200 and 202 as success.
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/load", strings.NewReader(`{"tenant_id":"t","source_uri":"file:///x"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-ID", "t")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /load: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	var body loader.LoadResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != loader.StatusRunning {
		t.Errorf("status = %q, want running", body.Status)
	}
	// Note: with the new contract the kit's job_id is overwritten by
	// the platform-issued transfer job_id when the transfer.Writer
	// closes. The stub's "job-123" lives on but the wire response
	// returns the writer's job_id. Either is acceptable proof the
	// async path works; we only assert the response was non-empty.
	if body.JobID == "" {
		t.Errorf("job_id was empty, expected platform-issued id")
	}
}

func TestLoad_MissingTenantID_Returns400(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{}
	client, _ := newTestClient(t, stub)

	_, err := client.Load(context.Background(), &loader.LoadRequest{
		// Missing TenantID — caught client-side before HTTP.
		SourceURI: "file:///x",
	})
	if err == nil {
		t.Fatal("expected error for missing tenant_id")
	}
	if !strings.Contains(err.Error(), "tenant_id") {
		t.Errorf("error = %v, want mention of tenant_id", err)
	}
	if stub.loadCalls.Load() != 0 {
		t.Errorf("load called %d times, want 0 (client-side validation)", stub.loadCalls.Load())
	}
}

func TestLoad_MissingTenantID_ServerSide_Returns400(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)

	// No X-Tenant-ID header — server rejects with missing_tenant_header.
	resp, err := http.Post(srv.URL+"/load", "application/json",
		strings.NewReader(`{"source_uri":"file:///x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var er loader.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Code != "missing_tenant_header" {
		t.Errorf("code = %q, want missing_tenant_header", er.Code)
	}
}

func TestLoad_TenantMismatch_Returns400(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)

	// Header says tenant-A, body claims tenant-B → reject.
	httpReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/load",
		strings.NewReader(`{"tenant_id":"tenant-B","source_uri":"file:///x"}`))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-ID", "tenant-A")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var er loader.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Code != "tenant_mismatch" {
		t.Errorf("code = %q, want tenant_mismatch", er.Code)
	}
	if stub.loadCalls.Load() != 0 {
		t.Errorf("load called %d times, want 0", stub.loadCalls.Load())
	}
}

func TestJob_Found(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{
		jobFn: func(_ context.Context, id string) (*loader.LoadResponse, error) {
			return &loader.LoadResponse{
				JobID:  id,
				Status: loader.StatusCompleted,
				Stats:  &loader.LoadStats{VerticesCreated: 5},
			}, nil
		},
	}
	client, _ := newTestClient(t, stub)

	resp, err := client.Job(context.Background(), "job-xyz")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if resp.Status != loader.StatusCompleted {
		t.Errorf("status = %q, want completed", resp.Status)
	}
	if resp.Stats.VerticesCreated != 5 {
		t.Errorf("vertices = %d, want 5", resp.Stats.VerticesCreated)
	}
}

func TestJob_NotFound_Returns404(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{
		jobFn: func(_ context.Context, id string) (*loader.LoadResponse, error) {
			return nil, &loader.ErrJobNotFound{JobID: id}
		},
	}
	client, _ := newTestClient(t, stub)

	_, err := client.Job(context.Background(), "missing-job")
	if err == nil {
		t.Fatal("expected error for missing job")
	}
	if !loader.IsNotFound(err) {
		t.Errorf("IsNotFound(err) = false, want true; err = %v", err)
	}
}

func TestJob_OtherError_Returns500(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{
		jobFn: func(_ context.Context, _ string) (*loader.LoadResponse, error) {
			return nil, errors.New("database explosion")
		},
	}
	client, _ := newTestClient(t, stub)

	_, err := client.Job(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *loader.HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if herr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", herr.StatusCode)
	}
}

func TestFormats(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{
		formatsFn: func(_ context.Context) ([]string, error) {
			return []string{"brick-campus-json", "ifc-step"}, nil
		},
	}
	client, _ := newTestClient(t, stub)

	formats, err := client.Formats(context.Background())
	if err != nil {
		t.Fatalf("Formats: %v", err)
	}
	if len(formats) != 2 || formats[0] != "brick-campus-json" {
		t.Errorf("formats = %v, want [brick-campus-json ifc-step]", formats)
	}
}

func TestHealth_OK(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{}
	client, _ := newTestClient(t, stub)

	hr, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if hr.Status != "ok" {
		t.Errorf("status = %q, want ok", hr.Status)
	}
}

func TestHealth_Unhealthy_Returns503(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{
		healthFn: func(_ context.Context) (*loader.HealthResponse, error) {
			return nil, errors.New("not ready")
		},
	}
	client, _ := newTestClient(t, stub)

	_, err := client.Health(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *loader.HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if herr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", herr.StatusCode)
	}
}

func TestLoad_InvalidJSON_Returns400(t *testing.T) {
	t.Parallel()
	stub := &stubHandler{}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)

	// Use real header so we hit the JSON decode path, not tenant rejection.
	httpReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/load", strings.NewReader(`{not json}`))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Tenant-ID", "tenant-1")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestNewClient_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"empty", "", true},
		{"no scheme", "loader.example.com:8400", true},
		{"valid", "http://loader.example.com:8400", false},
		{"valid with trailing path", "http://loader.example.com:8400/api/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loader.NewClient(tc.baseURL)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestJobStatus_IsTerminal(t *testing.T) {
	t.Parallel()
	terminal := []loader.JobStatus{loader.StatusCompleted, loader.StatusFailed, loader.StatusCancelled}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%q.IsTerminal() = false, want true", s)
		}
	}
	nonTerminal := []loader.JobStatus{loader.StatusPending, loader.StatusRunning}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%q.IsTerminal() = true, want false", s)
		}
	}
}
