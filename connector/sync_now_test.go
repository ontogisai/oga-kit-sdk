package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Sync now (OGA-1270): POST /connector/sync/{binding} runs one drain of a poll
// binding, single-flight with the ticker, and GET /connector/sync/{binding}
// reports the latest one.

// commitFactory builds real data writers over one fake commit client, whose job
// ids count up from job-1, so a test can check which batches a status lists.
func commitFactory() (WriterFactory, *transfer.FakeCommitClient) {
	var n atomic.Int32
	fc := &transfer.FakeCommitClient{JobIDFn: func() string {
		return fmt.Sprintf("job-%d", n.Add(1))
	}}
	return func(context.Context, Binding) (transfer.Writer, error) {
		return transfer.NewDataWriter(fc, "test-kit"), nil
	}, fc
}

func writeOne(em *Emitter) error {
	return em.Entities.WriteVertex(context.Background(), transfer.Vertex{EntityType: "Asset"})
}

func syncRequest(t *testing.T, method, url string) (int, SyncStatus) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var st SyncStatus
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode status: %v", err)
		}
	}
	return resp.StatusCode, st
}

// waitFinished polls GET until the latest drain is no longer running.
func waitFinished(t *testing.T, url string) SyncStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, st := syncRequest(t, http.MethodGet, url)
		if code != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", code)
		}
		if st.Outcome != SyncRunning {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the drain did not finish within 5s")
	return SyncStatus{}
}

func TestSyncNow_RunsOneDrainAndCommitsItsBatches(t *testing.T) {
	var cursors []string
	var mu sync.Mutex
	fc := &fakeConnector{
		bindings: []Binding{{ID: "assets", Mode: ModePoll}},
		syncFn: func(_ context.Context, _ Binding, cursor string, em *Emitter) (*SyncResult, error) {
			mu.Lock()
			cursors = append(cursors, cursor)
			mu.Unlock()
			if err := writeOne(em); err != nil {
				return nil, err
			}
			if cursor == "" {
				return &SyncResult{NextCursor: "c1", HasMore: true}, nil
			}
			return &SyncResult{NextCursor: "c2"}, nil
		},
	}
	factory, commits := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	url := srv.URL + SyncPath("assets")

	code, st := syncRequest(t, http.MethodPost, url)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	if st.Outcome != SyncRunning || st.Trigger != SyncTriggerManual || st.StartedAt == nil {
		t.Errorf("POST status = %+v, want a running manual drain with a start time", st)
	}

	st = waitFinished(t, url)
	if st.Outcome != SyncSucceeded || st.FinishedAt == nil || st.Error != "" {
		t.Fatalf("status = %+v, want a succeeded drain with a finish time", st)
	}
	if st.Batches != 2 || st.Records != 2 {
		t.Errorf("batches=%d records=%d, want 2 and 2 (one record per page)", st.Batches, st.Records)
	}
	if strings.Join(st.JobIDs, ",") != "job-1,job-2" {
		t.Errorf("job_ids = %v, want the two committed batches in order", st.JobIDs)
	}
	if commits.CompleteCalls() != 2 {
		t.Errorf("commits = %d, want 2", commits.CompleteCalls())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(cursors, ",") != ",c1" {
		t.Errorf("Sync cursors = %q, want one drain over both pages", cursors)
	}
}

func TestSyncNow_ConcurrentRequestAnswersInProgress(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	fc := &fakeConnector{
		bindings: []Binding{{ID: "assets"}}, // empty mode is poll
		syncFn: func(_ context.Context, _ Binding, _ string, em *Emitter) (*SyncResult, error) {
			calls.Add(1)
			<-release
			return &SyncResult{}, writeOne(em)
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	url := srv.URL + SyncPath("assets")

	code, first := syncRequest(t, http.MethodPost, url)
	if code != http.StatusAccepted {
		t.Fatalf("first POST = %d, want 202", code)
	}
	code, second := syncRequest(t, http.MethodPost, url)
	if code != http.StatusConflict {
		t.Fatalf("second POST = %d, want 409 while the first drain runs", code)
	}
	if second.Outcome != SyncRunning || second.StartedAt == nil || !second.StartedAt.Equal(*first.StartedAt) {
		t.Errorf("409 status = %+v, want the running drain's status", second)
	}

	// The ticker shares the rule: a tick while the manual drain runs is skipped.
	s.drainIfIdle(t.Context(), s.bindings["assets"], SyncTriggerPoll)

	close(release)
	if st := waitFinished(t, url); st.Outcome != SyncSucceeded || st.Trigger != SyncTriggerManual {
		t.Errorf("status = %+v, want the manual drain succeeded", st)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("Sync calls = %d, want 1: one drain ran, the request and the tick did not start another", n)
	}
}

func TestSyncNow_AnswersInProgressWhileTheTickerDrains(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "assets", Mode: ModeBoth}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			close(entered)
			<-release
			return &SyncResult{}, nil
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.drainIfIdle(context.Background(), s.bindings["assets"], SyncTriggerPoll)
	}()
	<-entered

	code, st := syncRequest(t, http.MethodPost, srv.URL+SyncPath("assets"))
	if code != http.StatusConflict || st.Trigger != SyncTriggerPoll {
		t.Errorf("POST during a tick = %d %+v, want 409 naming the poll drain", code, st)
	}
	close(release)
	<-done
}

// A failed drain reports its outcome, its error, and the job ids it committed
// before failing; the next drain starts again from the page that failed.
func TestSyncStatus_ReportsAFailedDrain(t *testing.T) {
	var failPage2 atomic.Bool
	failPage2.Store(true)
	var lastCursor atomic.Value
	fc := &fakeConnector{
		bindings: []Binding{{ID: "assets", Mode: ModePoll}},
		syncFn: func(_ context.Context, _ Binding, cursor string, em *Emitter) (*SyncResult, error) {
			lastCursor.Store(cursor)
			if cursor == "c1" && failPage2.Load() {
				_ = writeOne(em) // dropped with the failed page
				return nil, errors.New("upstream answered 502")
			}
			if err := writeOne(em); err != nil {
				return nil, err
			}
			if cursor == "" {
				return &SyncResult{NextCursor: "c1", HasMore: true}, nil
			}
			return &SyncResult{NextCursor: "c2"}, nil
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	url := srv.URL + SyncPath("assets")

	if code, _ := syncRequest(t, http.MethodPost, url); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	st := waitFinished(t, url)
	if st.Outcome != SyncFailed || !strings.Contains(st.Error, "upstream answered 502") || st.FinishedAt == nil {
		t.Fatalf("status = %+v, want a failed drain carrying the Sync error", st)
	}
	if st.Batches != 1 || st.Records != 1 || strings.Join(st.JobIDs, ",") != "job-1" {
		t.Errorf("status = %+v, want only page 1's committed batch (job-1)", st)
	}

	failPage2.Store(false)
	if code, _ := syncRequest(t, http.MethodPost, url); code != http.StatusAccepted {
		t.Fatalf("second POST = %d, want 202", code)
	}
	st = waitFinished(t, url)
	if st.Outcome != SyncSucceeded || st.Error != "" || strings.Join(st.JobIDs, ",") != "job-2" {
		t.Errorf("retry status = %+v, want a succeeded drain of the failed page alone (job-2)", st)
	}
	if got, _ := lastCursor.Load().(string); got != "c1" {
		t.Errorf("retry drained from %q, want c1: a failed page must not advance the cursor", got)
	}
}

func TestSyncNow_WebhookOnlyAndUnknownBindings404(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "push", Mode: ModeWebhook}, {ID: "pull", Mode: ModePoll}}}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	for _, id := range []string{"push", "nope"} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			if code, _ := syncRequest(t, method, srv.URL+SyncPath(id)); code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, SyncPath(id), code)
			}
		}
	}
}

func TestSyncStatus_BeforeAnyDrain(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "pull", Mode: ModePoll}}}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + SyncPath("pull"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"job_ids":[]`) || strings.Contains(string(body), `"outcome"`) {
		t.Errorf("body = %s, want no outcome and an empty (non-null) job_ids", body)
	}
}

func TestSyncNow_Refused503UntilConnectCompletes(t *testing.T) {
	var calls atomic.Int32
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			calls.Add(1)
			return &SyncResult{}, nil
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	s.connectPending.Store(true)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	if code, _ := syncRequest(t, http.MethodPost, srv.URL+SyncPath("pull")); code != http.StatusServiceUnavailable {
		t.Errorf("POST before Connect = %d, want 503", code)
	}
	if code, _ := syncRequest(t, http.MethodGet, srv.URL+SyncPath("pull")); code != http.StatusOK {
		t.Errorf("GET before Connect = %d, want 200: reading the status is not gated", code)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Sync calls = %d, want 0 before Connect", n)
	}
}

// Shutdown cancels a running sync-now drain and waits for it to return.
func TestListenAndServe_ShutdownWaitsForASyncNowDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	var returned atomic.Bool
	manualRunning := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		health:   map[string]Health{"pull": {OK: true}},
		syncFn: func(ctx context.Context, _ Binding, _ string, _ *Emitter) (*SyncResult, error) {
			if calls.Add(1) == 1 {
				return &SyncResult{}, nil // the first drain after Connect
			}
			close(manualRunning)
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // a Sync that takes a moment to stop
			returned.Store(true)
			return nil, ctx.Err()
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	factory, _ := commitFactory()
	cfg := &Config{
		Port:          port,
		WriterFactory: factory,
		PollInterval:  time.Hour,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, fc) }()

	// Wait for the first drain after Connect to finish, so the POST below is the
	// second Sync call.
	url := "http://127.0.0.1:" + port + SyncPath("pull")
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, st := tryRequest(http.MethodGet, url)
		if code == http.StatusOK && st.Trigger == SyncTriggerPoll && st.Outcome == SyncSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first poll drain never finished (last answer %d %+v)", code, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := tryRequest(http.MethodPost, url); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	<-manualRunning

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe = %v, want nil", err)
	}
	if !returned.Load() {
		t.Error("ListenAndServe returned before the sync-now drain did")
	}
}

// tryRequest reports a refused connection as status 0, for polling a server that
// is still starting.
func tryRequest(method, url string) (int, SyncStatus) {
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		return 0, SyncStatus{}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, SyncStatus{}
	}
	defer func() { _ = resp.Body.Close() }()
	var st SyncStatus
	_ = json.NewDecoder(resp.Body).Decode(&st)
	return resp.StatusCode, st
}

func TestPollRun_JobIDsKeepTheLatestAndErrorsAreCapped(t *testing.T) {
	r := newPollRuns(map[string]Binding{"b": {ID: "b"}})["b"]
	if _, _, ok := r.begin(SyncTriggerPoll); !ok {
		t.Fatal("begin on an idle binding must claim it")
	}
	total := MaxStatusJobIDs + 5
	for i := 1; i <= total; i++ {
		r.committed(fmt.Sprintf("job-%d", i), 2)
	}
	r.committed("", 1) // a writer that returned no receipt still counts
	r.finish("c9", errors.New(strings.Repeat("x", 2000)))

	st := r.snapshot()
	if st.Batches != total+1 || st.Records != 2*total+1 {
		t.Errorf("batches=%d records=%d, want %d and %d", st.Batches, st.Records, total+1, 2*total+1)
	}
	if len(st.JobIDs) != MaxStatusJobIDs || st.JobIDs[0] != "job-6" || st.JobIDs[MaxStatusJobIDs-1] != fmt.Sprintf("job-%d", total) {
		t.Errorf("job_ids = %d ids from %q, want the latest %d", len(st.JobIDs), st.JobIDs[0], MaxStatusJobIDs)
	}
	if len(st.Error) > 600 || !strings.HasSuffix(st.Error, "(truncated)") {
		t.Errorf("error is %d bytes, want it capped at 512 and marked truncated", len(st.Error))
	}
	if cursor, _, ok := r.begin(SyncTriggerManual); !ok || cursor != "c9" {
		t.Errorf("next drain starts at %q (claimed=%v), want c9", cursor, ok)
	}
}

// The cursor belongs to the binding, not to whichever trigger drains it: the
// ticker's next drain continues from where a sync-now drain stopped.
func TestSyncNow_TheTickerContinuesFromTheManualDrainsCursor(t *testing.T) {
	var lastCursor atomic.Value
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		syncFn: func(_ context.Context, _ Binding, cursor string, em *Emitter) (*SyncResult, error) {
			lastCursor.Store(cursor)
			return &SyncResult{NextCursor: cursor + "+"}, writeOne(em)
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	if code, _ := syncRequest(t, http.MethodPost, srv.URL+SyncPath("pull")); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	waitFinished(t, srv.URL+SyncPath("pull"))
	s.drainIfIdle(t.Context(), s.bindings["pull"], SyncTriggerPoll)
	if got, _ := lastCursor.Load().(string); got != "+" {
		t.Errorf("the tick drained from %q, want %q: the cursor the sync-now drain reached", got, "+")
	}
}

// A transport error quotes its URL, and a query string can be a credential: the
// status (and the log line) keep only the URL's location.
func TestSyncStatus_ErrorKeepsNoURLQuery(t *testing.T) {
	const secret = "access_token=s3cr3t"
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			return nil, fmt.Errorf("fetch page: %w", &url.Error{
				Op: "Get", URL: "https://upstream.example/items?" + secret, Err: errors.New("i/o timeout"),
			})
		},
	}
	factory, _ := commitFactory()
	var logged strings.Builder
	s := newTestServer(fc, factory)
	s.cfg.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	if code, _ := syncRequest(t, http.MethodPost, srv.URL+SyncPath("pull")); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	st := waitFinished(t, srv.URL+SyncPath("pull"))
	if st.Outcome != SyncFailed || !strings.Contains(st.Error, "https://upstream.example/items") {
		t.Fatalf("status = %+v, want the failed drain naming the URL's location", st)
	}
	if strings.Contains(st.Error, secret) || strings.Contains(logged.String(), secret) {
		t.Errorf("the URL's query reached the status (%q) or the log", st.Error)
	}
}

// Once shutdown has begun a drain would stop before its first page, so the
// request is refused rather than answered 202.
func TestSyncNow_Refused503OnceShutdownBegins(t *testing.T) {
	var calls atomic.Int32
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			calls.Add(1)
			return &SyncResult{}, nil
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.runCtx = ctx
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	if code, _ := syncRequest(t, http.MethodPost, srv.URL+SyncPath("pull")); code != http.StatusServiceUnavailable {
		t.Errorf("POST during shutdown = %d, want 503", code)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Sync calls = %d, want 0", n)
	}
}

// waitDrains closes the gate before it waits: a request arriving while a drain
// finishes is refused, and the wait returns only when that drain has returned.
func TestWaitDrains_RefusesNewDrainsAndWaitsForTheRunningOne(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			calls.Add(1)
			<-release
			return &SyncResult{}, nil
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	url := srv.URL + SyncPath("pull")

	if code, _ := syncRequest(t, http.MethodPost, url); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	waited := make(chan struct{})
	go func() { s.waitDrains(); close(waited) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _ := syncRequest(t, http.MethodPost, url)
		if code == http.StatusServiceUnavailable {
			break
		}
		if code != http.StatusConflict {
			t.Fatalf("POST while draining for shutdown = %d, want 409 until the gate closes, then 503", code)
		}
		if time.Now().After(deadline) {
			t.Fatal("POST still answers 409 after 5s: the gate never closed")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-waited:
		t.Fatal("waitDrains returned while a drain was still running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-waited
	if n := calls.Load(); n != 1 {
		t.Errorf("Sync calls = %d, want 1: no drain started after the gate closed", n)
	}
}

// A page that set WaitForCommit is committed before its wait, so a failed wait
// leaves a committed batch: the status lists its job and records the failure.
func TestSyncStatus_AFailedCommitWaitStillListsTheCommittedBatch(t *testing.T) {
	fc := &transfer.FakeCommitClient{}
	fc.StatusFn = func(_ context.Context, jobID string) (*transfer.StatusResponse, error) {
		return &transfer.StatusResponse{JobID: jobID, Status: transfer.JobFailed}, nil
	}
	impl := newPagedConnector(true)
	s, _ := commitWaitServer(impl, fc, fastCommitWait)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	url := srv.URL + SyncPath("b")

	if code, _ := syncRequest(t, http.MethodPost, url); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	st := waitFinished(t, url)
	if st.Outcome != SyncFailed || !strings.Contains(st.Error, "wait for committed batch") {
		t.Fatalf("status = %+v, want a drain failed by its commit wait", st)
	}
	if st.Batches != 1 || st.Records != 1 || len(st.JobIDs) != 1 || st.JobIDs[0] == "" {
		t.Errorf("status = %+v, want page 1's committed batch and its job id", st)
	}
	if got := impl.syncedCursors(); len(got) != 1 {
		t.Errorf("synced cursors = %q; the drain must stop at page 1", got)
	}
}
