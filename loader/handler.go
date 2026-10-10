package loader

import (
	"context"
	"errors"
)

// LoaderHandler is the interface a kit author implements to expose a
// data-load behavior over the loader HTTP contract. The SDK's
// [ListenAndServe] wraps an implementation, taking care of routing,
// JSON encoding, tenant header extraction, transfer-writer lifecycle,
// and shutdown.
//
// The handler is invoked from HTTP request handlers, so implementations
// must be safe to call from many goroutines. Loaders that maintain
// in-memory job state should serialize access internally.
//
// # Single-pass vs multi-pass
//
// Most kits implement only this interface and stream every record in a
// single pass over the source. Kits that need multiple passes (for
// example, building a source-id → platform-id resolution map in pass
// one, then emitting edges with resolved IDs in pass two) implement
// [StreamingLoaderHandler] instead. The SDK detects the streaming
// variant via type assertion at request time.
type LoaderHandler interface {
	// Load runs the load to completion. The handler emits records
	// through lc.Transfer (vertices, edges, entity types, hierarchy)
	// and the SDK closes the writer for it on success — kit code
	// must NOT call lc.Transfer.Close itself.
	//
	// The returned LoadResponse becomes the HTTP body. ListenAndServe
	// chooses the response code: 200 for terminal status, 202 for
	// non-terminal (async) status. Returning an error closes the
	// connection with HTTP 500.
	Load(ctx context.Context, lc *LoadContext) (*LoadResponse, error)

	// Job returns the current state of an async job by ID.
	// Synchronous loaders that never produce a job_id may return
	// [ErrJobNotFound]. The platform never calls Job for jobs that
	// completed inline.
	Job(ctx context.Context, jobID string) (*LoadResponse, error)

	// Formats returns the format identifiers this loader supports.
	// At least one entry MUST be present.
	Formats(ctx context.Context) ([]string, error)

	// Health reports whether the loader is ready to accept jobs.
	// Returning a non-nil error is treated by the platform as
	// unhealthy.
	Health(ctx context.Context) (*HealthResponse, error)
}

// StreamingLoaderHandler is the multi-pass variant of [LoaderHandler].
// Kits whose source format requires more than one walk over the input
// implement this interface; the SDK detects it via type assertion and
// drives the Plan / Pass loop instead of a single Load call.
//
// Embeds [LoaderHandler] so the basic methods (Job, Formats, Health)
// are reused. Load is still defined but conventionally returns an
// error directing callers to the streaming entry points; the SDK
// short-circuits to Plan / Pass when the handler implements this
// interface, so kits never need to implement Load when going
// multi-pass.
type StreamingLoaderHandler interface {
	LoaderHandler

	// Plan inspects the source and returns the passes the loader will
	// run. Called once per request, before any Pass call. The SDK's
	// driver loop calls Pass once per entry in plan.Passes, in slice
	// order.
	Plan(ctx context.Context, lc *LoadContext) (*LoadPlan, error)

	// Pass executes one pass. Implementations should write only the
	// record types they declared in the corresponding [PassSpec],
	// though the SDK does not enforce this.
	//
	// The lc.Transfer writer is shared across all passes — vertices
	// from pass 1 and edges from pass 2 land in the same artifact.
	// Kit code must NOT call lc.Transfer.Close in any pass; the SDK
	// closes the writer once after the final pass returns nil.
	Pass(ctx context.Context, lc *LoadContext, pass *PassSpec) error
}

// ErrJobNotFound is the canonical error returned by [LoaderHandler.Job]
// when the requested job ID is unknown. ListenAndServe maps this to
// HTTP 404.
//
// Loaders may wrap this with [fmt.Errorf] for additional context.
type ErrJobNotFound struct {
	JobID string
}

func (e *ErrJobNotFound) Error() string {
	return "job not found: " + e.JobID
}

// IsJobNotFound reports whether err is an ErrJobNotFound (including
// wrapped variants).
func IsJobNotFound(err error) bool {
	if err == nil {
		return false
	}
	var target *ErrJobNotFound
	return errors.As(err, &target)
}
