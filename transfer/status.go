package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/ontogisai/oga-kit-sdk/internal/redact"
)

// JobState is a job's lifecycle state as loader.status reports it.
type JobState string

// The states loader.status reports. Only [JobCompleted] and [JobFailed] are
// terminal.
const (
	JobQueued  JobState = "queued"
	JobRunning JobState = "running"
	// JobWaitingForOntology: a data job the platform holds before writing
	// anything, while the kit's ontology change is being applied.
	JobWaitingForOntology JobState = "waiting_for_ontology"
	JobCompleted          JobState = "completed"
	JobFailed             JobState = "failed"
)

// IsTerminal reports whether the job has finished: completed or failed.
func (s JobState) IsTerminal() bool {
	return s == JobCompleted || s == JobFailed
}

// JobStatus is the decoded answer of loader.status: one job as the platform
// last recorded it.
type JobStatus struct {
	// JobID is the job's platform-issued id, the [Receipt.JobID] of its commit.
	JobID string `json:"job_id"`

	// Status is the job's state.
	Status JobState `json:"status"`

	// Kind is the kind of the committed artifact.
	Kind LoadKind `json:"kind,omitempty"`

	// StartedAt is when the platform accepted the artifact.
	StartedAt time.Time `json:"started_at"`

	// CompletedAt is when the job reached a terminal state; zero before then.
	CompletedAt time.Time `json:"completed_at,omitzero"`

	// Error is the platform's reason for a failed job.
	Error string `json:"error,omitempty"`

	// Stats holds the counts the platform recorded for the job (records
	// written, unchanged and quarantined, by kind), as the platform sent them.
	// Decode the keys you need.
	Stats json.RawMessage `json:"stats,omitempty"`
}

// StatusClient reads a job's status through loader.status.
// [HTTPCommitClient] and [FakeCommitClient] implement it.
//
// It is separate from [CommitClient], which the writer needs, so a
// CommitClient implemented outside the SDK keeps compiling.
type StatusClient interface {
	// Status returns the job's current status. The platform answers only for
	// jobs of the caller's tenant.
	Status(ctx context.Context, jobID string) (*JobStatus, error)
}

// Defaults for [WaitOptions].
const (
	// DefaultWaitTimeout covers the longest a job can take on the platform:
	// a data job may be held for up to 10 minutes while its kit's ontology
	// change is applied, and its write may then run for up to 30 minutes.
	DefaultWaitTimeout = 45 * time.Minute

	// DefaultWaitPollInterval is the time between two loader.status calls.
	DefaultWaitPollInterval = time.Second
)

// WaitOptions bounds [WaitForJob]. Zero values take the defaults.
type WaitOptions struct {
	// Timeout is how long WaitForJob waits for the job to finish.
	// Default [DefaultWaitTimeout].
	Timeout time.Duration

	// PollInterval is the time between two status calls.
	// Default [DefaultWaitPollInterval].
	PollInterval time.Duration
}

func (o WaitOptions) withDefaults() WaitOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultWaitTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultWaitPollInterval
	}
	return o
}

// Errors [WaitForJob] wraps; test for them with [errors.Is].
var (
	// ErrJobFailed: the job finished as failed.
	ErrJobFailed = errors.New("transfer: job failed")

	// ErrJobWaitTimeout: the job did not finish within [WaitOptions.Timeout].
	ErrJobWaitTimeout = errors.New("transfer: timed out waiting for job")
)

// WaitForJob waits until the job of a commit has finished on the platform, and
// returns its last status.
//
// The platform writes each committed artifact on its own, in no order relative
// to the sidecar's other artifacts. A kit that needs one artifact written before
// it sends the next, for example the parents its next artifact's edges point
// at, waits for the first artifact's job before committing the second:
//
//	receipt, err := parents.Close(ctx)
//	if err != nil {
//		return err
//	}
//	if _, err := transfer.WaitForJob(ctx, client, receipt, transfer.WaitOptions{}); err != nil {
//		return err
//	}
//	// ... write and Close the next artifact.
//
// It checks the status at once, then every [WaitOptions.PollInterval]. A job
// that completed returns its status and nil. A job that failed returns its
// status and an error wrapping [ErrJobFailed]. A job still unfinished after
// [WaitOptions.Timeout] returns its last status, which may be nil, and an error
// wrapping [ErrJobWaitTimeout]. When ctx ends first the error wraps ctx's.
//
// A status call refused with 429, 502, 503 or 504, or one that never reached
// the platform, is tried again at the next poll. Any other refusal ends the
// wait with that error: a 401, 403 or 404 would get the same answer again, and
// the platform answers 500 for a job it does not have.
func WaitForJob(ctx context.Context, client StatusClient, receipt *Receipt, opts WaitOptions) (*JobStatus, error) {
	if client == nil {
		return nil, errors.New("transfer.WaitForJob: nil client")
	}
	if receipt == nil || receipt.JobID == "" {
		return nil, errors.New("transfer.WaitForJob: receipt has no job id")
	}
	opts = opts.withDefaults()
	jobID := receipt.JobID

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	var last *JobStatus
	var lastErr error
	for {
		st, err := client.Status(waitCtx, jobID)
		if err == nil && st == nil {
			err = errors.New("status client returned no status")
		}
		switch {
		case err == nil && st.Status == JobFailed:
			if st.Error == "" {
				return st, fmt.Errorf("%w: job %s", ErrJobFailed, jobID)
			}
			return st, fmt.Errorf("%w: job %s: %s", ErrJobFailed, jobID, redact.ErrorBody([]byte(st.Error)))
		case err == nil && st.Status.IsTerminal():
			return st, nil
		case err == nil:
			last, lastErr = st, nil
		case waitCtx.Err() != nil:
			// The call was cut short by the wait ending; reported below.
		case !retryableStatusError(err):
			return last, fmt.Errorf("transfer.WaitForJob: job %s: %w", jobID, err)
		default:
			lastErr = err
		}

		select {
		case <-waitCtx.Done():
			return last, waitEnded(ctx, jobID, opts.Timeout, last, lastErr)
		case <-ticker.C:
		}
	}
}

// waitEnded is the error for a wait whose context ended before the job did.
func waitEnded(parent context.Context, jobID string, timeout time.Duration, last *JobStatus, lastErr error) error {
	if err := parent.Err(); err != nil {
		return fmt.Errorf("transfer.WaitForJob: job %s: %w", jobID, err)
	}
	state := JobState("unknown")
	if last != nil {
		state = last.Status
	}
	if lastErr != nil {
		return fmt.Errorf("%w: job %s still %s after %s; last status call: %w",
			ErrJobWaitTimeout, jobID, state, timeout, lastErr)
	}
	return fmt.Errorf("%w: job %s still %s after %s", ErrJobWaitTimeout, jobID, state, timeout)
}

// retryableStatusError reports whether a failed status call may succeed when
// asked again: a rate limit, a gateway that could not reach or wait for the
// platform, or a call that did not get an answer.
func retryableStatusError(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &urlErr) || errors.As(err, &netErr)
}
