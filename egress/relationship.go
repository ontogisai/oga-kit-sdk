package egress

// Relationship-lane wire types (OGA-875, kg-egress-sync THIRD lane; origin
// SJ24K-31).
//
// This lane carries EDGE records — two independently-resolved endpoints — not
// entity instances or ontology type records. It exists because an owner
// reference (ParentRefs) is single-valued per declared edge, and a predicate
// like `feeds` is many_to_many in the kit's own ontology: it cannot be forced
// through the entity lane's owner-resolution machinery without either lying
// about cardinality or picking a target arbitrarily. See
// .kiro/specs/egress-relationship-lane/design.md for the full design.
//
// Reuses [Mode], [Outcome], [Correlation], [SyncResult] and [SyncResponse] from
// types.go: those are already record-kind-agnostic (a SyncResult's ID is
// whatever id the corresponding request record carried), so a relationship
// batch's reply is byte-for-byte the same shape a component already builds for
// the other two lanes. Only the REQUEST side needs a new shape, because a
// relationship carries two endpoints instead of one set of properties.

import "context"

// RelationshipEndpoint is one end of a relationship — a resolved
// (entity_id, entity_type, correlation) triple.
//
// Correlation is guaranteed non-nil with a non-empty ExternalRecordID by the
// time this reaches a component: an endpoint the platform could not correlate
// for the declaration's external_system fails that relationship before it is
// ever sent (OGA-EGRS-DATA-2005), so a component never has to defend against a
// nil Correlation here the way it must for Entity.Correlation on the entity
// lane (where nil legitimately means "not yet correlated, create it").
type RelationshipEndpoint struct {
	// EntityID is the platform's entity id for this endpoint. Useful for
	// logging; it is NOT the external system's id, which is Correlation's job.
	EntityID string `json:"entity_id"`

	// EntityType is the endpoint's own source-native class ID — the LEAF class,
	// which may differ from the declaration's source_type/target_type anchor
	// under a descendants-scoped entry. Matched exactly, never sanitized.
	EntityType string `json:"entity_type"`

	// Correlation is this endpoint's external-system identifier. See the type
	// doc comment: always present and non-empty by the time a component sees
	// it.
	Correlation *Correlation `json:"correlation"`
}

// Relationship is one edge as sent to the component.
type Relationship struct {
	// ID is the platform's edge id. Echo it back on the corresponding
	// SyncResult — never the external system's id, which belongs in that
	// result's ExternalRecordID.
	ID string `json:"id"`

	// Predicate is the batch's single relationship type — the homogeneity
	// label, matching the batch's own Predicate field on every record
	// (RelationshipSyncRequest is homogeneous, like SyncRequest).
	Predicate string `json:"predicate"`

	// Properties are the edge's own domain properties, if any. Most predicates
	// carry none; this exists so a kit that declares edge properties is not
	// blocked from reading them.
	Properties map[string]any `json:"properties,omitempty"`

	// Source and Target are the edge's two endpoints. Source is always the
	// edge's out() side, Target always in() — there is no direction ambiguity
	// to declare, unlike the entity lane's parent_edges direction, because a
	// relationship-lane declaration names source_type and target_type
	// explicitly.
	Source RelationshipEndpoint `json:"source"`
	Target RelationshipEndpoint `json:"target"`

	// Correlation carries this relationship's OWN external id, when one was
	// already recorded from a prior push. Its presence is how a component
	// knows to report `updated` instead of attempting a `created` that Core (or
	// any external system enforcing pair-uniqueness) may reject as a
	// duplicate.
	Correlation *Correlation `json:"correlation,omitempty"`
}

// RelationshipSyncRequest is the body of a push to
// POST /egress/relationship-sync.
//
// A batch is HOMOGENEOUS on (tenant, predicate, mode) — never a mixture, and
// in particular never a mixture of two declared (source_type, target_type)
// scope entries even when they share a predicate (a kit may declare `feeds`
// scoped to Equipment→Equipment in one entry and Equipment→Location in
// another; each is its own batch). A component's handler can therefore be a
// switch on Target.EntityType without worrying that two differently-scoped
// entries have been interleaved.
type RelationshipSyncRequest struct {
	// TenantID is INFORMATIONAL ONLY — see SyncRequest.TenantID for the same
	// rule.
	TenantID string `json:"tenant_id"`

	// ExternalSystem is the system-of-record key from the kit's manifest
	// declaration.
	ExternalSystem string `json:"external_system"`

	// Predicate is the single relationship type this batch carries, as the
	// SOURCE-NATIVE relationship type name — matching the manifest's
	// relationships_sync[].predicate exactly, verbatim (may contain a colon,
	// same rule as SyncRequest.EntityType).
	Predicate string `json:"predicate"`

	// Mode is bulk (Day-1) or change (Day-2). Day-2 relationship delivery is
	// not implemented by the platform as of this lane's introduction — see the
	// design's Non-Goals — so a component only ever receives ModeBulk here
	// today, but the field exists so a future Day-2 wiring needs no wire
	// change.
	Mode Mode `json:"mode"`

	// BatchID is STABLE ACROSS RETRIES, exactly like SyncRequest.BatchID. It
	// additionally encodes the declared scope entry (source_type/target_type),
	// not just the predicate — two scope entries sharing a predicate would
	// otherwise mint colliding batch ids across entries, and a component that
	// deduplicates on batch_id would silently replay one entry's verdicts for
	// the other's records.
	BatchID string `json:"batch_id"`

	// Relationships are the edges to push, in the platform's push order.
	Relationships []Relationship `json:"relationships"`
}

// newRelationshipBatch builds a RelationshipBatch for the relationships of one
// request. Mirrors newBatch exactly, keyed on Relationship.ID instead of
// Entity.ID.
func newRelationshipBatch(rels []Relationship) *RelationshipBatch {
	b := &RelationshipBatch{
		ids:     make([]string, 0, len(rels)),
		known:   make(map[string]struct{}, len(rels)),
		results: make(map[string]SyncResult, len(rels)),
	}
	for i := range rels {
		id := rels[i].ID
		if id == "" {
			continue
		}
		if _, dup := b.known[id]; dup {
			continue
		}
		b.known[id] = struct{}{}
		b.ids = append(b.ids, id)
	}
	return b
}

// RelationshipBatch collects a component's per-relationship verdicts for one
// push and builds a response the platform will accept.
//
// This is [Batch]'s exact counterpart for the relationship lane: same
// acceptance rule (an omitted, repeated, unrequested, or malformed-outcome
// verdict discards the WHOLE batch on the platform side — OGA-EGRS-DATA-2001,
// generalized to the third lane with no new code), same per-relationship
// (never per-batch) failure isolation, same normalization of a component bug
// into a per-record Failed rather than propagating it. See [Batch] for the
// full rationale; it is not repeated here because the two types would drift
// out of sync with their own doc comments otherwise.
//
// A RelationshipBatch is NOT safe for concurrent use, exactly like Batch.
type RelationshipBatch struct {
	ids     []string
	known   map[string]struct{}
	results map[string]SyncResult
	defects []string
}

// Created records that the component created an external record for a
// relationship id. externalRecordID is REQUIRED — see [Batch.Created].
func (b *RelationshipBatch) Created(id, externalRecordID string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeCreated, ExternalRecordID: externalRecordID})
}

// Updated records that the component updated the existing external record for
// a relationship id. externalRecordID is REQUIRED — see [Batch.Updated].
func (b *RelationshipBatch) Updated(id, externalRecordID string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeUpdated, ExternalRecordID: externalRecordID})
}

// Skipped records that this relationship needs no external record.
//
// This is the right verdict for an unmapped predicate (Requirement 5.2 of the
// design): a predicate the kit's mapping table does not recognize is
// deliberately declined, never guessed onto one of the external system's
// enum values, and Skipped is what keeps that a SUCCESS rather than a
// failure.
func (b *RelationshipBatch) Skipped(id string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeSkipped})
}

// Failed records a per-relationship failure. See [Batch.Failed] — the same
// rule applies: this fails THIS relationship only, and an empty reason is
// replaced with a placeholder rather than left blank.
func (b *RelationshipBatch) Failed(id, reason string) {
	if reason == "" {
		reason = "component reported a failure without a reason"
	}
	b.record(SyncResult{ID: id, Outcome: OutcomeFailed, Error: reason})
}

// FailedErr is [RelationshipBatch.Failed] with an error value.
func (b *RelationshipBatch) FailedErr(id string, err error) {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	b.Failed(id, reason)
}

// Record stores an arbitrary verdict. Prefer the named helpers.
func (b *RelationshipBatch) Record(r SyncResult) { b.record(r) }

func (b *RelationshipBatch) record(r SyncResult) {
	if _, ok := b.known[r.ID]; !ok {
		b.defects = append(b.defects, "verdict for id "+r.ID+", which was not in the batch (dropped)")
		return
	}
	if _, dup := b.results[r.ID]; dup {
		b.defects = append(b.defects, "duplicate verdict for id "+r.ID+" (last one wins)")
	}
	b.results[r.ID] = r
}

// Pending returns the requested relationship ids that have no verdict yet, in
// request order.
func (b *RelationshipBatch) Pending() []string {
	out := make([]string, 0, len(b.ids)-len(b.results))
	for _, id := range b.ids {
		if _, ok := b.results[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Len reports the number of relationships in the batch.
func (b *RelationshipBatch) Len() int { return len(b.ids) }

// Results returns one verdict per requested relationship id, in request
// order, alongside the component-bug descriptions collected while building
// it. See [Batch.Results] for the full normalization rules — identical here,
// substituting "relationship" for "entity".
func (b *RelationshipBatch) Results() ([]SyncResult, []string) {
	defects := b.defects
	out := make([]SyncResult, 0, len(b.ids))
	for _, id := range b.ids {
		r, ok := b.results[id]
		if !ok {
			defects = append(defects, "no verdict for requested id "+id)
			out = append(out, SyncResult{
				ID: id, Outcome: OutcomeFailed,
				Error: "component returned no verdict for this relationship",
			})
			continue
		}
		if !r.Outcome.Valid() {
			defects = append(defects, "unrecognized outcome "+string(r.Outcome)+" for id "+id)
			out = append(out, SyncResult{
				ID: id, Outcome: OutcomeFailed,
				Error: "component reported unrecognized outcome " + string(r.Outcome),
			})
			continue
		}
		if (r.Outcome == OutcomeCreated || r.Outcome == OutcomeUpdated) && r.ExternalRecordID == "" {
			defects = append(defects, "outcome "+string(r.Outcome)+" for id "+id+" carries no external_record_id")
			out = append(out, SyncResult{
				ID: id, Outcome: OutcomeFailed,
				Error: "component reported " + string(r.Outcome) + " without an external_record_id",
			})
			continue
		}
		out = append(out, r)
	}
	return out, defects
}

// RelationshipSyncer is implemented by a component that also serves the
// RELATIONSHIPS lane — pushing edge records with two independently-resolved
// endpoints.
//
// It is OPTIONAL and separate from [Component], on the same reasoning
// [OntologyTypeSyncer] gives: a kit whose manifest declares no
// relationships_sync block has nothing to push, and implementing this
// interface IS the statement "this component serves that lane".
//
// # Pin the method signature at compile time
//
// This is a runtime type assertion — see [OntologyTypeSyncer]'s doc comment
// for why that matters and how to guard against a signature typo:
//
//	var _ egress.RelationshipSyncer = (*myComponent)(nil)
type RelationshipSyncer interface {
	// SyncRelationships pushes one homogeneous batch of relationship records
	// and records a verdict per relationship on b.
	//
	// The verdict rules are [Component.Sync]'s, unchanged: nil when the batch
	// was PROCESSED (per-relationship failures belong on b), an error only for
	// a batch-wide fault, and a [ThrottleError] to pass an external system's
	// backpressure through.
	SyncRelationships(ctx context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error
}
