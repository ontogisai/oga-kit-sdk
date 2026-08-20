package egress

// Wire types for the egress HTTP contract. These mirror the platform's
// internal/egress contract structs field-for-field, including every JSON tag —
// the two live in different repositories, so the tags ARE the contract. See
// wire_golden_test.go for the drift guard.

// Mode distinguishes the two pushes a component may receive. It is passed
// through so a component can, for example, log or rate-limit a bulk load
// differently from a trickle of changes. The platform's own behavior does not
// branch on it, so a component is free to ignore it entirely.
type Mode string

const (
	// ModeBulk is the resumable Day-1 load of everything currently in the graph.
	ModeBulk Mode = "bulk"
	// ModeChange is the Day-2 push of entities that changed.
	ModeChange Mode = "change"
)

// Outcome is a component's verdict for one entity in a batch.
type Outcome string

const (
	// OutcomeCreated and OutcomeUpdated mean the external record exists and the
	// component is reporting its id. These are the ONLY outcomes that produce a
	// correlation write on the platform side, and both REQUIRE a non-empty
	// ExternalRecordID — a verdict claiming a record exists but leaving it
	// unaddressable is rejected as malformed, because the next update could not
	// route to it.
	OutcomeCreated Outcome = "created"
	OutcomeUpdated Outcome = "updated"

	// OutcomeSkipped is a SUCCESS: this entity needs no external record, or is
	// already current. No correlation is written and nothing is failed, so a run
	// full of skips is a clean run rather than a silent error. Prefer it over
	// OutcomeFailed for "nothing to do".
	OutcomeSkipped Outcome = "skipped"

	// OutcomeFailed is a per-entity failure and fails that entity ONLY — the
	// other entities in the batch are still correlated. Use it for a genuine
	// per-record problem (the external system rejected this payload), never for
	// a batch-wide fault: signal those by returning an error from Sync, which
	// lets the platform retry the whole batch.
	OutcomeFailed Outcome = "failed"
)

// Valid reports whether o is one of the four recognized outcomes. The platform
// rejects an unrecognized outcome as a malformed response, so the server checks
// this before replying rather than letting a typo reach the platform.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeCreated, OutcomeUpdated, OutcomeSkipped, OutcomeFailed:
		return true
	default:
		return false
	}
}

// Correlation is the (system, record id) pair already recorded for an entity.
type Correlation struct {
	ExternalSystem   string `json:"external_system"`
	ExternalRecordID string `json:"external_record_id"`
}

// Entity is one entity as received from the platform. Properties are the
// entity's domain properties as read from the knowledge graph; the platform does
// not interpret them, and mapping them to the external system's shape is the
// component's job.
type Entity struct {
	// ID is the platform's entity id. It is what a SyncResult must echo back —
	// never the external system's id, which belongs in ExternalRecordID.
	ID string `json:"id"`
	// EntityType is the source-native class ID, matching the batch's EntityType
	// (a batch is homogeneous). See SyncRequest.EntityType for the matching rule.
	EntityType string         `json:"entity_type"`
	Properties map[string]any `json:"properties,omitempty"`

	// Correlation carries the external id already recorded for this entity, when
	// there is one. Its presence is how a component knows to UPDATE rather than
	// CREATE — which is what makes a re-run of a completed sync all updates
	// instead of a second set of duplicate external records. A component that
	// ignores it will duplicate records on every re-run.
	Correlation *Correlation `json:"correlation,omitempty"`

	// ParentRefs carries the entity's OWNER for each edge the kit declared under
	// entity_types[].parent_edges, keyed by that edge name.
	//
	// This is the only way to populate an external foreign key. The entity as
	// read carries no containment — containment is an edge, and an entity read
	// projects columns — so without this a component has nothing identifying what
	// contains the record it is about to create.
	//
	// Direction is the record's parent or container, never its children.
	//
	// The platform guarantees three things, so a component does not have to
	// defend against them: each entry is SINGLE-VALUED (a declared edge
	// resolving to several targets fails the batch rather than picking one); a
	// present entry's ExternalRecordID is non-empty (a parent not yet pushed
	// fails the batch, so a null foreign key is never sent); and the key set is
	// exactly the declared parent_edges. An ABSENT entry means the entity is a
	// root of that relation — omit the foreign key, do not treat it as an error.
	ParentRefs map[string]ParentRef `json:"parent_refs,omitempty"`
}

// ParentRef identifies one resolved owner of a pushed entity.
type ParentRef struct {
	// EntityID is the owner's platform entity id. Useful for logging and for a
	// component keeping its own map; it is NOT the external system's id.
	EntityID string `json:"entity_id"`

	// ExternalRecordID is the owner's id IN THE EXTERNAL SYSTEM — the value a
	// foreign key needs. It is non-empty whenever the entry is present, because
	// the platform pushes and correlates a parent before any of its children.
	ExternalRecordID string `json:"external_record_id"`
}

// SyncRequest is the body of POST /egress/sync.
//
// A batch is HOMOGENEOUS: one (tenant, entity_type, mode) per call, never a
// mixture. A component may therefore map one target shape per call, and its
// handler stays a switch rather than a per-item dispatcher.
type SyncRequest struct {
	// TenantID is INFORMATIONAL ONLY. A component's authority over tenancy is
	// its own workload identity — the platform never reads a tenant back out of
	// a response, and never grants a component reach beyond the tenant it was
	// deployed for. Log it, assert against it, but do not treat it as a
	// credential or use it to select a tenant's credentials.
	TenantID string `json:"tenant_id"`

	// ExternalSystem is the system-of-record key from the kit's manifest
	// declaration, echoed here so a component serving several systems can route
	// on it.
	ExternalSystem string `json:"external_system"`

	// EntityType is the single type this batch carries, as the SOURCE-NATIVE
	// CLASS ID — the identifier the source system uses, verbatim.
	//
	// It may contain a colon (`brick:AHU`, `rec:Zone`) and it may equally be
	// colon-free (`Equipment`, `WorkOrder`); both forms are class IDs, and a
	// colon-free one is not a "plainer" spelling of a namespaced one — they are
	// different catalog entries. Match it EXACTLY as received and route on the
	// whole string. Do not sanitize it, normalize the case, split on the colon, or
	// map it to some tidier internal name: the platform performs no translation
	// outbound, so this value is already the customer-facing identifier, and the
	// manifest's entity_types[] entry it corresponds to is the same string.
	//
	// It is NEVER the platform's internal storage identifier. Where rows physically
	// live is a platform concern the contract does not expose.
	EntityType string `json:"entity_type"`

	// Mode is bulk (Day-1) or change (Day-2).
	Mode Mode `json:"mode"`

	// BatchID is STABLE ACROSS RETRIES. The platform retries a transient push
	// failure with the same batch id, and a transport failure means the request
	// may have been fully processed before the response was lost — so a
	// component that records batch ids can deduplicate a redelivery instead of
	// creating a second external record for every entity in it.
	BatchID string `json:"batch_id"`

	// Entities are the entities to push, in the platform's push order.
	Entities []Entity `json:"entities"`
}

// SyncResult is a component's verdict for one requested entity.
type SyncResult struct {
	// ID MUST be the platform entity id from the corresponding request entity —
	// never the external system's id, which belongs in ExternalRecordID.
	ID string `json:"id"`

	// Outcome is the verdict. See the Outcome constants.
	Outcome Outcome `json:"outcome"`

	// ExternalRecordID is the external system's identifier for this entity.
	// REQUIRED for created/updated; the platform persists it as the entity's
	// correlation.
	ExternalRecordID string `json:"external_record_id,omitempty"`

	// Error is a human-readable per-entity reason, for failed outcomes. It
	// reaches the operator's run report, so make it actionable.
	Error string `json:"error,omitempty"`
}

// SyncResponse is the body of a POST /egress/sync reply.
//
// It MUST carry exactly one result per requested entity id. The platform
// validates this and, on any mismatch — a missing id, an unrequested id, a
// duplicate, an unknown outcome, or a created/updated verdict with no external
// record id — discards the WHOLE batch and persists nothing
// (OGA-EGRS-DATA-2001). Partially trusting such a reply risks writing one
// entity's external record id onto another, which every later update would then
// propagate. [Batch.Results] builds a conforming response so a component does
// not have to track this by hand.
type SyncResponse struct {
	Results []SyncResult `json:"results"`
}

// EntityTypesResponse is the body of the optional GET /egress/entity-types
// introspection endpoint.
//
// Advisory only — the MANIFEST is authoritative for what gets pushed. This
// exists so an operator can see what a component believes it supports; a
// disagreement is a kit-authoring bug to surface, never something the platform
// silently reconciles.
type EntityTypesResponse struct {
	EntityTypes []string `json:"entity_types"`
}
