package connector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// pagedConnector serves two pages from cursor "": page 1 carries the parent,
// page 2 its child. waitOnFirst sets WaitForCommit on page 1; onPage2 runs
// when page 2 is synced, before anything of it is committed.
type pagedConnector struct {
	*fakeConnector
	mu          sync.Mutex
	syncs       []string
	waitOnFirst bool
	onPage2     func()
}

func newPagedConnector(waitOnFirst bool) *pagedConnector {
	p := &pagedConnector{waitOnFirst: waitOnFirst}
	p.fakeConnector = &fakeConnector{
		bindings: []Binding{{ID: "b"}},
		syncFn: func(ctx context.Context, _ Binding, cursor string, em *Emitter) (*SyncResult, error) {
			p.mu.Lock()
			p.syncs = append(p.syncs, cursor)
			p.mu.Unlock()
			switch cursor {
			case "":
				if err := em.Entities.WriteVertex(ctx, transfer.Vertex{ID: "parent", EntityType: "ex:Site"}); err != nil {
					return nil, err
				}
				return &SyncResult{NextCursor: "p1", HasMore: true, WaitForCommit: p.waitOnFirst}, nil
			default:
				if p.onPage2 != nil {
					p.onPage2()
				}
				if err := em.Entities.WriteEdge(ctx, transfer.Edge{
					RelationshipType: "ex:isPartOf", SourceID: "child", TargetID: "parent",
				}); err != nil {
					return nil, err
				}
				return &SyncResult{NextCursor: "p2"}, nil
			}
		},
	}
	return p
}

func (p *pagedConnector) syncedCursors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.syncs...)
}

// commitWaitServer is a server whose writers commit through fc, which also
// answers the waits, and whose log is returned.
func commitWaitServer(impl SourceConnector, fc *transfer.FakeCommitClient, wait transfer.WaitOptions) (*server, *bytes.Buffer) {
	s := newTestServer(impl, func(context.Context, Binding) (transfer.Writer, error) {
		return transfer.NewDataWriter(fc, "kit-a"), nil
	})
	var logs bytes.Buffer
	s.cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	s.cfg.StatusClient = fc
	s.cfg.CommitWait = wait
	return s, &logs
}

// fastCommitWait polls every millisecond; its timeout is far above what any
// test here needs, except the one about the timeout.
var fastCommitWait = transfer.WaitOptions{PollInterval: time.Millisecond, Timeout: 5 * time.Second}

func TestDrain_WaitForCommit_NextPageOnlyAfterTheJobIsTerminal(t *testing.T) {
	var mu sync.Mutex
	terminal := map[string]bool{}
	polls := 0
	fc := &transfer.FakeCommitClient{}
	fc.StatusFn = func(_ context.Context, jobID string) (*transfer.StatusResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls < 3 {
			return &transfer.StatusResponse{JobID: jobID, Status: transfer.JobRunning}, nil
		}
		terminal[jobID] = true
		return &transfer.StatusResponse{JobID: jobID, Status: transfer.JobCompleted}, nil
	}
	impl := newPagedConnector(true)
	var page2SawTerminal, page2SawCommits bool
	impl.onPage2 = func() {
		mu.Lock()
		defer mu.Unlock()
		page2SawTerminal = terminal["job-1"]
		page2SawCommits = fc.CompleteCalls() == 1
	}
	s, logs := commitWaitServer(impl, fc, fastCommitWait)

	final := s.drain(t.Context(), impl.bindings[0], "")

	if final != "p2" {
		t.Errorf("final cursor = %q, want p2", final)
	}
	if !page2SawTerminal || !page2SawCommits {
		t.Errorf("page 2 was synced before page 1's job was terminal (terminal=%v, page 1 committed=%v)",
			page2SawTerminal, page2SawCommits)
	}
	if fc.CompleteCalls() != 2 {
		t.Errorf("commits = %d, want 2", fc.CompleteCalls())
	}
	if fc.StatusCalls() != 3 {
		t.Errorf("status calls = %d, want 3 (page 1 only: page 2 does not ask to wait)", fc.StatusCalls())
	}
	log := logs.String()
	if !strings.Contains(log, `"msg":"source connector waited for a committed batch"`) ||
		!strings.Contains(log, `"job_id":"job-1"`) || !strings.Contains(log, `"wait_ms":`) {
		t.Errorf("the wait must be logged with its job id and duration; log:\n%s", log)
	}
}

func TestDrain_WithoutWaitForCommit_DoesNotWait(t *testing.T) {
	fc := &transfer.FakeCommitClient{}
	impl := newPagedConnector(false)
	s, _ := commitWaitServer(impl, fc, fastCommitWait)

	if final := s.drain(t.Context(), impl.bindings[0], ""); final != "p2" {
		t.Errorf("final cursor = %q, want p2", final)
	}
	if fc.CompleteCalls() != 2 || fc.StatusCalls() != 0 {
		t.Errorf("commits = %d, status calls = %d; want 2 and 0", fc.CompleteCalls(), fc.StatusCalls())
	}
}

func TestDrain_FailedOrTimedOutWaitKeepsTheCursor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   transfer.JobState
		wait    transfer.WaitOptions
		wantErr error
	}{
		{name: "failed job", state: transfer.JobFailed, wait: fastCommitWait, wantErr: transfer.ErrJobFailed},
		{
			name: "timeout", state: transfer.JobRunning,
			wait:    transfer.WaitOptions{PollInterval: time.Millisecond, Timeout: 20 * time.Millisecond},
			wantErr: transfer.ErrJobWaitTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &transfer.FakeCommitClient{}
			fc.StatusFn = func(_ context.Context, jobID string) (*transfer.StatusResponse, error) {
				return &transfer.StatusResponse{JobID: jobID, Status: tc.state, Error: "persist failed"}, nil
			}
			impl := newPagedConnector(true)
			s, logs := commitWaitServer(impl, fc, tc.wait)

			if final := s.drain(t.Context(), impl.bindings[0], ""); final != "" {
				t.Errorf("cursor = %q, want it unchanged", final)
			}
			if got := impl.syncedCursors(); len(got) != 1 {
				t.Errorf("synced cursors = %q; the drain must stop at page 1", got)
			}
			if fc.CompleteCalls() != 1 {
				t.Errorf("commits = %d, want 1 (page 2 never committed)", fc.CompleteCalls())
			}
			if !strings.Contains(logs.String(), `"job_id":"job-1"`) {
				t.Errorf("the failed wait must be logged with its job id; log:\n%s", logs.String())
			}
			if _, err := s.runSync(t.Context(), impl.bindings[0], ""); !errors.Is(err, tc.wantErr) {
				t.Errorf("runSync err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRunSync_WaitForCommitWithoutStatusClientCommitsNothing(t *testing.T) {
	fc := &transfer.FakeCommitClient{}
	impl := newPagedConnector(true)
	s, _ := commitWaitServer(impl, fc, fastCommitWait)
	s.cfg.StatusClient = nil

	_, err := s.runSync(t.Context(), impl.bindings[0], "")
	if err == nil || !strings.Contains(err.Error(), "StatusClient") {
		t.Errorf("err = %v, want the missing StatusClient named", err)
	}
	if fc.CompleteCalls() != 0 {
		t.Errorf("commits = %d, want 0: a batch whose wait cannot run is not committed", fc.CompleteCalls())
	}
}

func TestRunSync_WaitForCommitOnAnEmptyPageWaitsForNothing(t *testing.T) {
	fc := &transfer.FakeCommitClient{}
	impl := &fakeConnector{
		bindings: []Binding{{ID: "b"}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			return &SyncResult{NextCursor: "c1", WaitForCommit: true}, nil
		},
	}
	s, _ := commitWaitServer(impl, fc, fastCommitWait)

	res, err := s.runSync(t.Context(), impl.bindings[0], "")
	if err != nil || res.NextCursor != "c1" {
		t.Fatalf("runSync = %+v, %v", res, err)
	}
	if fc.CompleteCalls() != 0 || fc.StatusCalls() != 0 {
		t.Errorf("commits = %d, status calls = %d; want 0 and 0", fc.CompleteCalls(), fc.StatusCalls())
	}
}
