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

// OntologyTypeSyncer is implemented by a component that also serves the ONTOLOGY
// lane — pushing the tenant's ontology TYPE records into the external system's
// reference tables, so an entity's own type has an external identifier to
// reference before the entity is pushed.
//
// It is OPTIONAL and separate from [Component] on purpose. A kit whose manifest
// declares no ontology_sync block has no catalogue to push, and forcing every
// component to implement a method it will never serve — returning an error, or
// worse, silently doing nothing — is a weaker statement than not implementing it.
// Implementing this interface IS the statement "this component serves that lane".
//
// # The record kind is the ENDPOINT, never an inference
//
// Type records arrive at POST /egress/ontology-sync and entities at
// POST /egress/sync. The two payloads are otherwise the same shape, and their
// batch labels legitimately COLLIDE: a kit that declares the anchor Equipment in
// both lanes sends both under entity_type "Equipment", which is the intended
// declaration (a kit pushes the Equipment catalogue and the Equipment instances).
// Before this split a component had to recover the kind from the batch's content,
// and misreading it writes into the customer's system of record — type records
// into the asset register, or instances into the classification table. Routing
// answers the question instead, which also makes a MIXED batch unrepresentable:
// the kind cannot vary per entity when it is a property of the URL.
//
// # Pin the method signature at compile time
//
// This is a runtime type assertion, so a typo in the signature does not fail to
// compile — it silently means "not implemented", and the server answers 501. Add
// the standard assertion next to the implementation so the compiler catches it:
//
//	var _ egress.OntologyTypeSyncer = (*myComponent)(nil)
type OntologyTypeSyncer interface {
	// SyncOntologyTypes pushes one homogeneous batch of ontology type records and
	// records a verdict per record on b.
	//
	// The verdict rules are [Component.Sync]'s, unchanged: nil when the batch was
	// PROCESSED (per-record failures belong on b), an error only for a batch-wide
	// fault, and a [ThrottleError] to pass an external system's backpressure
	// through. The platform persists each returned identifier onto the type
	// record itself, which is what makes a re-run an update rather than a
	// duplicate create.
	//
	// req.EntityType is the batch's ANCHOR — the storage type whose catalogue
	// this batch belongs to — not the record's own name. A record's own name is
	// its business key for the external system and travels as a property, because
	// that is the value the external system stores and matches on.
	SyncOntologyTypes(ctx context.Context, req *SyncRequest, b *Batch) error
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
