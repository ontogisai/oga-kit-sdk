package fetch

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"
)

// RetryConfig tunes bounded exponential backoff. It mirrors the platform's retry
// shape without importing anything platform-internal, so a kit gets the same
// semantics without a dependency it cannot have.
type RetryConfig struct {
	MaxAttempts   int
	InitialDelay  time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	Jitter        bool
}

// DefaultRetry is the policy for fetching a customer-published artifact: a few
// patient attempts to ride out a briefly-slow customer endpoint, then give up so
// the caller's NEXT cycle retries.
//
// Deliberately shallow. A connector that keeps retrying inside one cycle holds a
// worker and delays the trigger that would have re-read the source anyway; for a
// full-snapshot connector the next cycle is the natural retry, and it starts from
// a fresh URL rather than a possibly-expired presigned one.
var DefaultRetry = RetryConfig{
	MaxAttempts:   3,
	InitialDelay:  500 * time.Millisecond,
	MaxDelay:      5 * time.Second,
	BackoffFactor: 2.0,
	Jitter:        true,
}

// permanentError marks an error that must not be retried.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks err as non-retryable, so Do stops immediately instead of
// spending the whole backoff budget on something that cannot succeed.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err was marked non-retryable.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Do runs fn with bounded exponential backoff, stopping early on a permanent
// error or a cancelled context. On exhaustion it returns the last error wrapped
// with the attempt count.
//
// Exported so a kit can apply one policy to its own calls (submitting to a
// platform intake, say) rather than hand-rolling a second backoff loop whose
// pacing differs from the fetcher's.
func Do(ctx context.Context, cfg RetryConfig, fn func() error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	delay := cfg.InitialDelay
	var last error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = fn()
		if last == nil {
			return nil
		}
		var perm *permanentError
		if errors.As(last, &perm) {
			return perm.err
		}
		if !isRetryable(last) {
			return last
		}
		if attempt == cfg.MaxAttempts {
			break
		}
		wait := delay
		if cfg.Jitter && delay > 0 {
			wait += time.Duration(rand.Int64N(int64(delay)/2 + 1))
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		if next := time.Duration(float64(delay) * cfg.BackoffFactor); next > cfg.MaxDelay {
			delay = cfg.MaxDelay
		} else {
			delay = next
		}
	}
	return fmt.Errorf("operation failed after %d attempts: %w", cfg.MaxAttempts, last)
}

// isRetryable classifies an error. Context cancellation and deadline are
// terminal; a net timeout is retryable; anything not explicitly permanent
// defaults to retryable, i.e. transient-until-proven-otherwise.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var perm *permanentError
	if errors.As(err, &perm) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return true
}

// httpError carries a response status so a caller can branch on it.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// StatusError maps a non-2xx response to an error, marking 4xx permanent (a
// client or contract error that will recur identically) and 5xx retryable.
func StatusError(status int, body string) error {
	msg := fmt.Sprintf("HTTP %d", status)
	if body != "" {
		msg = fmt.Sprintf("%s: %s", msg, body)
	}
	err := &httpError{status: status, msg: msg}
	if status >= 400 && status < 500 {
		return Permanent(err)
	}
	return err
}

// StatusOf returns the HTTP status embedded in err, or 0 when err carries none.
func StatusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return 0
}
