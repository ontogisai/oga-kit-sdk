package loader

import (
	"context"
	"errors"
)

// errorsAs is a thin shim around errors.As that returns a bool for
// readability. Kept package-private — callers use [IsJobNotFound].
func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}

// LoaderHandler is the interface a kit author implements to expose data-load
// behavior over the loader HTTP contract. The SDK's [ListenAndServe] wraps
// an implementation, taking care of routing, JSON encoding, and shutdown.
//
// The handler is invoked from HTTP request handlers, so implementations must
// be safe to call from many goroutines. Loaders that maintain in-memory job
// state should serialize access internally.
type LoaderHandler interface {
	// Load starts a load job. Implementations may run synchronously and
	// return Status=completed inline, or kick off an async job and return
	// Status=running with a job_id. When async, the platform polls Job.
	//
	// The returned LoadResponse becomes the HTTP body. ListenAndServe
	// chooses the response code: 200 for terminal status, 202 for
	// non-terminal (async) status.
	Load(ctx context.Context, req *LoadRequest) (*LoadResponse, error)

	// Job returns the current state of an async job by ID. Synchronous
	// loaders that never produce a job_id may return ErrJobNotFound.
	// The platform never calls Job for jobs that completed inline.
	Job(ctx context.Context, jobID string) (*LoadResponse, error)

	// Formats returns the format identifiers this loader supports. At
	// least one entry MUST be present. Used for capability discovery
	// and validation at import-submit time.
	Formats(ctx context.Context) ([]string, error)

	// Health reports whether the loader is ready to accept jobs.
	// Returning a non-nil error is treated by the platform as unhealthy.
	// Implementations typically check their own goroutine pool, output
	// pipeline, or downstream connections.
	Health(ctx context.Context) (*HealthResponse, error)
}

// ErrJobNotFound is the canonical error returned by [LoaderHandler.Job] when
// the requested job ID is unknown. ListenAndServe maps this to HTTP 404.
//
// Loaders may wrap this with [fmt.Errorf] for additional context.
type ErrJobNotFound struct {
	JobID string
}

func (e *ErrJobNotFound) Error() string {
	return "job not found: " + e.JobID
}

// IsJobNotFound reports whether err is an ErrJobNotFound (including wrapped).
func IsJobNotFound(err error) bool {
	if err == nil {
		return false
	}
	var target *ErrJobNotFound
	return errorsAs(err, &target)
}
