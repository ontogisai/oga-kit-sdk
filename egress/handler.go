package egress

import "context"

// Component is the interface a kit author implements to expose an egress
// behavior over the egress HTTP contract. [ListenAndServe] wraps an
// implementation, taking care of the routes, request decoding, response
// construction, and graceful shutdown.
//
// Implementations must be safe for concurrent calls: the platform may push
// several batches of a non-hierarchical type in parallel (the manifest's
// max_in_flight), and each arrives on its own HTTP goroutine. A component that
// keeps shared state must serialize access to it.
type Component interface {
	// Connect establishes credentials and verifies connectivity to the external
	// system. Called ONCE at startup, before any Sync. Returning an error aborts
	// startup, which is deliberate: a component that cannot reach its external
	// system should fail its readiness rather than accept batches it will fail
	// one entity at a time.
	Connect(ctx context.Context) error

	// Sync pushes one homogeneous batch to the external system and records a
	// verdict per entity on b.
	//
	// Return nil when the batch was PROCESSED, even if some entities failed —
	// per-entity failures belong on b (b.Failed), and returning nil is what lets
	// the platform persist the correlations of the entities that succeeded.
	//
	// Return an error ONLY for a batch-wide fault (the external system is down,
	// authentication expired, the whole request was rejected). That fails the
	// batch as a unit and the platform retries it with the SAME batch_id, so any
	// work already done must be deduplicable — see SyncRequest.BatchID.
	Sync(ctx context.Context, req *SyncRequest, b *Batch) error

	// Health reports whether the component can currently reach its external
	// system. The platform's sidecar health monitor probes GET /healthz and
	// removes an unhealthy component from routing, so report honestly: claiming
	// health while the external system is unreachable converts a visible outage
	// into a run that fails every entity.
	Health(ctx context.Context) Health
}

// EntityTypeLister is an OPTIONAL interface a component implements to serve
// GET /egress/entity-types.
//
// Advisory only — the kit MANIFEST is authoritative for what the platform
// pushes, and the platform never reconciles a disagreement by pushing a
// different set than was declared. Implement it so an operator can see what the
// component believes it supports; when it is not implemented the endpoint
// answers 404 and nothing on the platform's push path is affected.
type EntityTypeLister interface {
	EntityTypes(ctx context.Context) []string
}

// Health is a component's health report, served as the JSON body of
// GET /healthz. The platform's monitor keys on the HTTP status only, so the
// fields are for the operator reading the response.
type Health struct {
	// OK is true when the component is connected and able to push. False makes
	// /healthz answer 503.
	OK bool `json:"ok"`

	// Message is an optional human-readable detail — the reason when not OK, or
	// progress information when OK.
	Message string `json:"message,omitempty"`
}
