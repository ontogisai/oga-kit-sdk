package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ontogisai/oga-kit-sdk/internal/redact"
)

// Sync now: run one drain of a poll binding on demand, and report the binding's
// latest drain (OGA-1270).
//
// A poll binding otherwise drains only on its ticker, so an operator who has just
// changed the source waits a whole PollInterval and cannot see whether the last
// poll worked. The ticker and POST /sync/{binding} share one drain per binding:
// whichever starts first holds the binding until it finishes, and the other is
// told so (a tick is skipped, a request answers 409). Sync is therefore never
// called concurrently for one binding, and the cursor stays with the binding
// rather than with whoever drains it.

// PathSync is the route of a poll binding's sync now (POST) and of its latest
// drain's status (GET). [SyncPath] builds it for one binding.
const PathSync = "/sync/{binding}"

// SyncPath is [PathSync] for one binding id, escaped for a URL.
func SyncPath(binding string) string {
	return "/sync/" + url.PathEscape(binding)
}

// SyncOutcome is the state of a binding's latest drain.
type SyncOutcome string

const (
	// SyncRunning: the drain has started and not finished.
	SyncRunning SyncOutcome = "running"
	// SyncSucceeded: the drain consumed every page the connector had (HasMore=false).
	SyncSucceeded SyncOutcome = "succeeded"
	// SyncFailed: the drain stopped early. SyncStatus.Error says why.
	SyncFailed SyncOutcome = "failed"
)

// SyncTrigger is what started a drain.
type SyncTrigger string

const (
	// SyncTriggerPoll is the binding's ticker, including the first drain after
	// Connect.
	SyncTriggerPoll SyncTrigger = "poll"
	// SyncTriggerManual is POST /sync/{binding}.
	SyncTriggerManual SyncTrigger = "manual"
)

// MaxStatusJobIDs bounds [SyncStatus.JobIDs]. A first drain of a large source can
// commit thousands of batches; the status keeps the latest ones, which bracket a
// failure, and Batches keeps the count.
const MaxStatusJobIDs = 100

// SyncStatus is a poll binding's latest drain: the one running, or else the last
// one to finish. A new drain replaces it when it starts.
//
// It is held in memory, so a restarted connector reports no drain until its
// first one. Webhook deliveries to a binding that also polls are not drains and
// do not appear here.
type SyncStatus struct {
	// Binding is the binding id.
	Binding string `json:"binding"`

	// Outcome is running, succeeded or failed. Empty when the binding has not
	// been drained since the connector started.
	Outcome SyncOutcome `json:"outcome,omitempty"`

	// Trigger is what started the drain.
	Trigger SyncTrigger `json:"trigger,omitempty"`

	// StartedAt is when the drain started (UTC).
	StartedAt *time.Time `json:"started_at,omitempty"`

	// FinishedAt is when it finished (UTC). Absent while it runs.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Records counts the records in the batches the drain committed: what Sync
	// wrote through Emitter.Entities. Points written to Emitter.Timeseries are
	// not counted, and neither is a batch a Sync error dropped.
	Records int `json:"records"`

	// Batches counts the committed batches, one per Sync page that wrote a record.
	Batches int `json:"batches"`

	// JobIDs are the platform job ids of the committed batches, in commit order:
	// the last MaxStatusJobIDs when Batches is larger. Never null.
	JobIDs []string `json:"job_ids"`

	// Error is why a failed drain stopped, cut at 512 bytes. A transport
	// error's URL (*url.Error) is reduced to its location: no userinfo, query
	// or fragment. Batches committed before the failure stay committed and are
	// listed in JobIDs; the next drain starts again from the page that failed.
	Error string `json:"error,omitempty"`
}

// errCursorNotAdvanced ends a drain whose Sync reported more pages without
// moving its cursor: calling it again would replay the same page forever.
var errCursorNotAdvanced = errors.New("connector reported HasMore without advancing its cursor; drain stopped")

// pollRun is one poll binding's drain state. mu guards every field. A drain
// holds the binding (running) from begin to finish, and only that drain reads
// and advances the cursor.
type pollRun struct {
	mu      sync.Mutex
	running bool
	cursor  string
	status  SyncStatus
}

// newPollRuns makes the drain state of every poll-enabled binding.
func newPollRuns(bindings map[string]Binding) map[string]*pollRun {
	runs := make(map[string]*pollRun, len(bindings))
	for id, b := range bindings {
		if b.Mode.pollEnabled() {
			runs[id] = &pollRun{status: SyncStatus{Binding: id, JobIDs: []string{}}}
		}
	}
	return runs
}

// begin claims the binding for one drain and returns the cursor it starts from
// and its status. When a drain is already running it returns false and the
// running drain's status.
func (r *pollRun) begin(trigger SyncTrigger) (string, SyncStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return "", r.snapshotLocked(), false
	}
	now := time.Now().UTC()
	r.running = true
	r.status = SyncStatus{
		Binding:   r.status.Binding,
		Outcome:   SyncRunning,
		Trigger:   trigger,
		StartedAt: &now,
		JobIDs:    []string{},
	}
	return r.cursor, r.snapshotLocked(), true
}

// committed records one batch the running drain committed.
func (r *pollRun) committed(jobID string, records int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Batches++
	r.status.Records += records
	if jobID == "" {
		return // a writer that returned no receipt
	}
	r.status.JobIDs = append(r.status.JobIDs, jobID)
	if n := len(r.status.JobIDs); n > MaxStatusJobIDs {
		r.status.JobIDs = r.status.JobIDs[n-MaxStatusJobIDs:]
	}
}

// finish ends the running drain at cursor and records its outcome.
func (r *pollRun) finish(cursor string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.cursor = cursor
	r.running = false
	r.status.FinishedAt = &now
	if err != nil {
		r.status.Outcome = SyncFailed
		r.status.Error = redact.Truncate(err.Error(), redact.MaxQuotedBody)
		return
	}
	r.status.Outcome = SyncSucceeded
}

// snapshot returns a copy of the status, safe to encode outside the lock.
func (r *pollRun) snapshot() SyncStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *pollRun) snapshotLocked() SyncStatus {
	st := r.status
	st.JobIDs = append([]string{}, r.status.JobIDs...)
	return st
}

// pollTarget resolves a binding id to a poll-enabled binding and its drain
// state. False for an unknown or webhook-only binding.
func (s *server) pollTarget(id string) (Binding, *pollRun, bool) {
	b, ok := s.bindings[id]
	if !ok || !b.Mode.pollEnabled() {
		return Binding{}, nil, false
	}
	run := s.polls[id]
	return b, run, run != nil
}

// drainIfIdle runs one drain of b unless one is running, which it leaves to
// finish. The poll loop's path: a tick that finds a sync-now drain running is
// skipped, since that drain reads the same changes.
func (s *server) drainIfIdle(ctx context.Context, b Binding, trigger SyncTrigger) {
	run := s.polls[b.ID]
	if run == nil {
		return
	}
	cursor, _, ok := run.begin(trigger)
	if !ok {
		return
	}
	s.drainHeld(ctx, b, run, cursor)
}

// drainHeld drains b from cursor for the caller that holds run, then releases it
// with the cursor reached and the outcome.
func (s *server) drainHeld(ctx context.Context, b Binding, run *pollRun, cursor string) {
	next, err := s.drain(ctx, b, cursor, run.committed)
	run.finish(next, err)
}

// handleSyncNow serves POST /sync/{binding}. It starts one drain of a poll
// binding and answers 202 with its status, or 409 with the running drain's
// status when one is already running (from the ticker or an earlier request).
//
// The drain runs after the response, on the server's lifetime context: a caller
// that disconnects does not cut it short, and shutdown cancels it and waits for
// it. Refused with 503 until the initial Connect attempt has completed, because
// Sync is never called before Connect returns, and once shutdown has begun,
// when the drain would stop before its first page.
func (s *server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	b, run, ok := s.pollTarget(r.PathValue("binding"))
	if !ok {
		http.Error(w, "unknown or non-poll binding", http.StatusNotFound)
		return
	}
	if s.connectPending.Load() {
		http.Error(w, "connector is still completing its initial Connect; retry shortly",
			http.StatusServiceUnavailable)
		return
	}
	if s.runCtx.Err() != nil {
		http.Error(w, "connector is shutting down", http.StatusServiceUnavailable)
		return
	}
	cursor, st, started := run.begin(SyncTriggerManual)
	if !started {
		writeSyncStatus(w, http.StatusConflict, st)
		return
	}
	s.drains.Go(func() { s.drainHeld(s.runCtx, b, run, cursor) })
	s.cfg.Logger.Info("source connector sync now started", "binding", b.ID)
	writeSyncStatus(w, http.StatusAccepted, st)
}

// handleSyncStatus serves GET /sync/{binding}: the poll binding's latest drain.
func (s *server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	_, run, ok := s.pollTarget(r.PathValue("binding"))
	if !ok {
		http.Error(w, "unknown or non-poll binding", http.StatusNotFound)
		return
	}
	writeSyncStatus(w, http.StatusOK, run.snapshot())
}

func writeSyncStatus(w http.ResponseWriter, code int, st SyncStatus) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(st)
}
