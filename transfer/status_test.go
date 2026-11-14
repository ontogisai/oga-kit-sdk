package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// fastWait keeps the wait tests quick: a 1 ms poll and a timeout far above
// what any of them needs, unless the test is about the timeout.
var fastWait = transfer.WaitOptions{PollInterval: time.Millisecond, Timeout: 5 * time.Second}

func TestHTTPCommitClient_Status(t *testing.T) {
	t.Parallel()
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(body)
		// The platform's loader.status answer for a completed data job.
		_, _ = io.WriteString(w, `{"job_id":"job-7","status":"completed","kind":"data",`+
			`"started_at":"2026-11-14T08:00:00Z","completed_at":"2026-11-14T08:00:02Z","elapsed_ms":2000,`+
			`"content_hash":"abc","entry_count":3,"stats":{"vertices_created":2,"edges_created":1}}`)
	}))
	t.Cleanup(srv.Close)
	cc, err := transfer.NewHTTPCommitClient(srv.URL, "tenant-a", "kit-a",
		transfer.WithTokenProvider(func() string { return "tok" }))
	if err != nil {
		t.Fatal(err)
	}

	st, err := cc.Status(t.Context(), " job-7 ")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if gotPath != "/loader/v1/status" || gotAuth != "Bearer tok" || gotBody != `{"job_id":"job-7"}` {
		t.Errorf("request: path %q auth %q body %s", gotPath, gotAuth, gotBody)
	}
	if st.JobID != "job-7" || st.Status != transfer.JobCompleted || st.Kind != transfer.KindData {
		t.Errorf("status = %+v", st)
	}
	if want := time.Date(2026, 11, 14, 8, 0, 2, 0, time.UTC); !st.CompletedAt.Equal(want) {
		t.Errorf("CompletedAt = %v, want %v", st.CompletedAt, want)
	}
	var stats struct {
		VerticesCreated int `json:"vertices_created"`
	}
	if err := json.Unmarshal(st.Stats, &stats); err != nil || stats.VerticesCreated != 2 {
		t.Errorf("stats = %s (%v)", st.Stats, err)
	}
}

func TestHTTPCommitClient_StatusRefusals(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	answer := func(code int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	client := func(srv *httptest.Server) *transfer.HTTPCommitClient {
		cc, err := transfer.NewHTTPCommitClient(srv.URL, "tenant-a", "kit-a")
		if err != nil {
			t.Fatal(err)
		}
		return cc
	}

	before := calls.Load()
	if _, err := client(answer(http.StatusOK, `{}`)).Status(t.Context(), "  "); err == nil {
		t.Error("an empty job id must be refused")
	}
	if calls.Load() != before {
		t.Error("an empty job id must not reach the platform")
	}

	if _, err := client(answer(http.StatusOK, `{"job_id":"j"}`)).Status(t.Context(), "j"); err == nil ||
		!strings.Contains(err.Error(), "missing status") {
		t.Errorf("an answer without a status: err = %v", err)
	}

	_, err := client(answer(http.StatusForbidden, `{"error":"tool_not_allowed"}`)).Status(t.Context(), "j")
	var httpErr *transfer.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden || httpErr.Path != "/loader/v1/status" {
		t.Errorf("a refusal: err = %v", err)
	}
}

func TestJobState_IsTerminal(t *testing.T) {
	t.Parallel()
	for state, want := range map[transfer.JobState]bool{
		transfer.JobQueued:             false,
		transfer.JobRunning:            false,
		transfer.JobWaitingForOntology: false,
		transfer.JobCompleted:          true,
		transfer.JobFailed:             true,
		"":                             false,
		"cancelled":                    false,
	} {
		if got := state.IsTerminal(); got != want {
			t.Errorf("%q.IsTerminal() = %v, want %v", state, got, want)
		}
	}
}

// scripted answers Status with each step in turn, repeating the last one.
type scripted struct {
	calls atomic.Int32
	steps []func(jobID string) (*transfer.JobStatus, error)
}

func (s *scripted) Status(_ context.Context, jobID string) (*transfer.JobStatus, error) {
	i := int(s.calls.Add(1)) - 1
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	return s.steps[i](jobID)
}

func state(st transfer.JobState) func(string) (*transfer.JobStatus, error) {
	return func(jobID string) (*transfer.JobStatus, error) {
		return &transfer.JobStatus{JobID: jobID, Status: st}, nil
	}
}

func failure(err error) func(string) (*transfer.JobStatus, error) {
	return func(string) (*transfer.JobStatus, error) { return nil, err }
}

func TestWaitForJob_ReturnsOnceTheJobCompletes(t *testing.T) {
	t.Parallel()
	s := &scripted{steps: []func(string) (*transfer.JobStatus, error){
		state(transfer.JobQueued), state(transfer.JobWaitingForOntology), state(transfer.JobRunning), state(transfer.JobCompleted),
	}}
	st, err := transfer.WaitForJob(t.Context(), s, &transfer.Receipt{JobID: "job-1"}, fastWait)
	if err != nil {
		t.Fatalf("WaitForJob: %v", err)
	}
	if st.Status != transfer.JobCompleted || st.JobID != "job-1" {
		t.Errorf("status = %+v", st)
	}
	if n := s.calls.Load(); n != 4 {
		t.Errorf("status calls = %d, want 4 (no state before completed is terminal)", n)
	}
}

func TestWaitForJob_FailedJobIsAnError(t *testing.T) {
	t.Parallel()
	s := &scripted{steps: []func(string) (*transfer.JobStatus, error){
		state(transfer.JobRunning),
		func(jobID string) (*transfer.JobStatus, error) {
			return &transfer.JobStatus{JobID: jobID, Status: transfer.JobFailed, Error: "persist: " + strings.Repeat("x", 2000)}, nil
		},
	}}
	st, err := transfer.WaitForJob(t.Context(), s, &transfer.Receipt{JobID: "job-1"}, fastWait)
	if !errors.Is(err, transfer.ErrJobFailed) {
		t.Fatalf("err = %v, want ErrJobFailed", err)
	}
	if st == nil || st.Status != transfer.JobFailed {
		t.Errorf("a failed job returns its status, got %+v", st)
	}
	if msg := err.Error(); !strings.Contains(msg, "job-1") || !strings.Contains(msg, "persist: ") || len(msg) > 700 {
		t.Errorf("error names the job and quotes a bounded reason; got %d bytes: %.120s", len(msg), msg)
	}
}

func TestWaitForJob_TimesOut(t *testing.T) {
	t.Parallel()
	s := &scripted{steps: []func(string) (*transfer.JobStatus, error){state(transfer.JobRunning)}}
	start := time.Now()
	st, err := transfer.WaitForJob(t.Context(), s, &transfer.Receipt{JobID: "job-1"},
		transfer.WaitOptions{Timeout: 30 * time.Millisecond, PollInterval: time.Millisecond})
	if !errors.Is(err, transfer.ErrJobWaitTimeout) {
		t.Fatalf("err = %v, want ErrJobWaitTimeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("the wait's own timeout must not read as the caller's deadline")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %v, want the last state", err)
	}
	if st == nil || st.Status != transfer.JobRunning {
		t.Errorf("a timed-out wait returns the last status, got %+v", st)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the wait took %s, past its 30ms timeout", elapsed)
	}
}

func TestWaitForJob_RetriesATransientStatusFailure(t *testing.T) {
	t.Parallel()
	s := &scripted{steps: []func(string) (*transfer.JobStatus, error){
		failure(&transfer.HTTPError{StatusCode: http.StatusTooManyRequests, Path: "/loader/v1/status"}),
		failure(&transfer.HTTPError{StatusCode: http.StatusServiceUnavailable, Path: "/loader/v1/status"}),
		failure(&transfer.HTTPError{StatusCode: http.StatusGatewayTimeout, Path: "/loader/v1/status"}),
		failure(&url.Error{Op: "Post", URL: "http://gateway/loader/v1/status", Err: errors.New("connection refused")}),
		state(transfer.JobCompleted),
	}}
	st, err := transfer.WaitForJob(t.Context(), s, &transfer.Receipt{JobID: "job-1"}, fastWait)
	if err != nil || st.Status != transfer.JobCompleted {
		t.Fatalf("WaitForJob = %+v, %v; want completed after the transient failures", st, err)
	}
	if n := s.calls.Load(); n != 5 {
		t.Errorf("status calls = %d, want 5", n)
	}
}

func TestWaitForJob_TimeoutNamesTheLastStatusFailure(t *testing.T) {
	t.Parallel()
	s := &scripted{steps: []func(string) (*transfer.JobStatus, error){
		failure(&transfer.HTTPError{StatusCode: http.StatusBadGateway, Path: "/loader/v1/status"}),
	}}
	_, err := transfer.WaitForJob(t.Context(), s, &transfer.Receipt{JobID: "job-1"},
		transfer.WaitOptions{Timeout: 20 * time.Millisecond, PollInterval: time.Millisecond})
	var httpErr *transfer.HTTPError
	if !errors.Is(err, transfer.ErrJobWaitTimeout) || !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway {
		t.Errorf("err = %v, want ErrJobWaitTimeout wrapping the 502", err)
	}
}

func TestWaitForJob_PermanentRefusalEndsTheWait(t *testing.T) {
	t.Parallel()
	// 500 included: the platform answers it for a job it does not have.
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		s := &scripted{steps: []func(string) (*transfer.JobStatus, error){
			failure(&transfer.HTTPError{StatusCode: code, Path: "/loader/v1/status"}),
			state(transfer.JobCompleted),
		}}
		_, err := transfer.WaitForJob(t.Context(), s, &transfer.Receipt{JobID: "job-1"}, fastWait)
		var httpErr *transfer.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != code || errors.Is(err, transfer.ErrJobWaitTimeout) {
			t.Errorf("%d: err = %v, want the refusal itself", code, err)
		}
		if n := s.calls.Load(); n != 1 {
			t.Errorf("%d: status calls = %d, want 1 (asking again gets the same answer)", code, n)
		}
	}
}

func TestWaitForJob_CallerCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s := &scripted{steps: []func(string) (*transfer.JobStatus, error){
		func(jobID string) (*transfer.JobStatus, error) {
			cancel()
			return &transfer.JobStatus{JobID: jobID, Status: transfer.JobRunning}, nil
		},
	}}
	_, err := transfer.WaitForJob(ctx, s, &transfer.Receipt{JobID: "job-1"}, fastWait)
	if !errors.Is(err, context.Canceled) || errors.Is(err, transfer.ErrJobWaitTimeout) {
		t.Errorf("err = %v, want the caller's cancellation", err)
	}
}

func TestWaitForJob_BadArguments(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	if _, err := transfer.WaitForJob(t.Context(), nil, &transfer.Receipt{JobID: "j"}, fastWait); err == nil {
		t.Error("a nil client must be refused")
	}
	for _, r := range []*transfer.Receipt{nil, {}} {
		if _, err := transfer.WaitForJob(t.Context(), fc, r, fastWait); err == nil {
			t.Errorf("receipt %+v must be refused", r)
		}
	}
	nilStatus := &scripted{steps: []func(string) (*transfer.JobStatus, error){
		func(string) (*transfer.JobStatus, error) { return nil, nil },
	}}
	if _, err := transfer.WaitForJob(t.Context(), nilStatus, &transfer.Receipt{JobID: "j"}, fastWait); err == nil {
		t.Error("a client answering no status and no error must not be waited on forever")
	}
	if fc.StatusCalls() != 0 {
		t.Errorf("status calls = %d, want 0", fc.StatusCalls())
	}
}

func TestFakeCommitClient_Status(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewDataWriter(fc, "kit-a")
	if err := w.WriteVertex(t.Context(), transfer.Vertex{ID: "a", EntityType: "ex:Pump"}); err != nil {
		t.Fatal(err)
	}
	receipt, err := w.Close(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.JobID != "job-1" {
		t.Errorf("first job id = %q, want job-1", receipt.JobID)
	}
	st, err := fc.Status(t.Context(), receipt.JobID)
	if err != nil || st.Status != transfer.JobCompleted {
		t.Errorf("Status of a minted job = %+v, %v; want completed", st, err)
	}
	if _, err := fc.Status(t.Context(), "job-unknown"); err == nil {
		t.Error("a job the fake never minted must be an error")
	}
	if fc.StatusCalls() != 2 {
		t.Errorf("StatusCalls = %d, want 2", fc.StatusCalls())
	}
}
