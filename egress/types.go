package egress

import (
	"fmt"
	"strings"
	"unicode"
)

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

	// ParentRefs carries the entity's resolved OWNERS, keyed by how each was
	// reached.
	//
	// This is the only way to populate an external foreign key. The entity as
	// read carries no containment — containment is an edge, and an entity read
	// projects columns — so without this a component has nothing identifying what
	// contains the record it is about to create.
	//
	// THREE KINDS OF KEY can appear, and a component reads all three the same way
	// (one map, one value shape):
	//
	//   - An EDGE NAME, for each edge the kit declared under
	//     entities_sync[].parent_edges. Direction is the record's parent or
	//     container, never its children.
	//   - [TypeRefKey], when the entity's type sets entities_sync[].type_ref —
	//     the entity's own ontology TYPE record, correlated. Not an edge.
	//   - [OntologyParentRefKey], on an ontology_sync batch — the PARENT TYPE of
	//     the type record being pushed. Not an edge either.
	//
	// The platform guarantees two things, so a component does not have to defend
	// against them: each entry is SINGLE-VALUED (a declared edge resolving to
	// several targets fails the batch rather than picking one), and a present
	// entry's ExternalRecordID is non-empty (an owner not yet pushed and
	// correlated fails the batch, so a null foreign key is never sent). An ABSENT
	// entry means the entity is a root of that relation — omit the foreign key, do
	// not treat it as an error.
	//
	// The key set is NOT simply the declared parent_edges. Earlier revisions of
	// this comment said it was, which stopped being true when type_ref and the
	// ontology lane landed; a component written against that sentence would
	// conclude a type reference cannot be in this map.
	ParentRefs map[string]ParentRef `json:"parent_refs,omitempty"`

	// TypeAncestry is this entity's type chain as the tenant's LIVE ontology
	// defines it, most-specific FIRST: the entity's own class ID, then its parent,
	// then its grandparent, up to the root.
	//
	// SELF-INCLUSIVE, which the name alone does not settle: a rec:Building whose
	// only ancestor is rec:Space arrives as ["rec:Building", "rec:Space"], not
	// ["rec:Space"]. So a component matching "is this one of the roots I declared"
	// needs no special case for an entity that IS a root.
	//
	// ⚠ READ THIS INSTEAD OF Properties["class_hierarchy"]. The two look
	// interchangeable and are not. This field is resolved by the platform from the
	// ontology of record. `class_hierarchy` is a domain PROPERTY written by the data
	// loader from the source export's own class block — a different source — so when
	// that block is absent the loader falls back to a chain naming only the leaf,
	// and a synthesized vertex may carry no such property at all. A router walking
	// the property therefore finds no declared root for exactly the entities at the
	// top of the hierarchy, which is the failure this field was added to remove.
	//
	// ABSENT (omitted) means the platform has no ancestry to state: the entity's
	// class ID is not in the tenant's active ontology, which a stale row left by a
	// re-labeling import can produce. It is deliberately NOT flattened to a
	// one-element chain naming the leaf, because that is byte-identical to a genuine
	// root. Treat absence as unknown and fail the entity if you need the ancestry —
	// do not infer one.
	//
	// ⚠ A PRESENT chain is complete only as far as the tenant's ontology is. It ends
	// at the first ancestor the ontology does not define — which a RETIRED type
	// produces — and such a chain is indistinguishable from one that ended at a real
	// root. So "my declared root is not in this chain" can mean the entity is outside
	// your hierarchy OR that an ancestor was retired from the ontology. Both are real
	// gaps in the ontology rather than in the payload: fail the entity and report the
	// chain you received, do not widen the match or fall back to class_hierarchy.
	//
	// ENTITY LANE ONLY. On an ontology_sync batch the EntityType is the anchor
	// rather than the type record's own name, so a chain resolved from it would
	// describe the anchor; that lane expresses hierarchy through
	// ParentRefs[OntologyParentRefKey].
	//
	// Mirrors the platform's internal/egress.Entity.TypeAncestry.
	TypeAncestry []string `json:"type_ancestry,omitempty"`
}

// Reserved [Entity.ParentRefs] keys. Neither is an edge name: they name how the
// owner was reached rather than a predicate traversed to reach it, which is why
// they can share one map with the edge-keyed entries without a direction
// qualifier.
//
// They are declared HERE, in the contract package, because a component has to
// match them byte-for-byte to read the reference — and a wire literal that a kit
// retypes from prose is a literal that drifts. The platform holds the same two
// constants on its side.
const (
	// TypeRefKey is the key an entity's own correlated ontology TYPE arrives
	// under, when the kit sets entities_sync[].type_ref on that entity type.
	//
	// For 24K Core the entry's ExternalRecordID IS Asset.asset_classification_id
	// (or AssetDataPoint.asset_datapoint_name_id) — indistinguishable in handling
	// from the space_id an edge-resolved owner supplies.
	//
	// Setting type_ref makes the entry MANDATORY, not optional: if the type was
	// never pushed and correlated, the platform fails the batch rather than
	// sending an entity with a null reference. So a component may read it
	// directly; an absent entry here is a platform-side failure the component
	// never sees.
	//
	// ⚠ RESERVED. A kit that declares an owner edge literally named "type_ref" on
	// a type that also sets type_ref collides on this key, and nothing currently
	// rejects that — the two values are written by different code paths, so the
	// collision presents as a silently overwritten reference rather than an error.
	// Do not name an edge this.
	TypeRefKey = "type_ref"

	// OntologyParentRefKey is the key a type record's PARENT TYPE arrives under,
	// on a batch produced by an ontology_sync anchor with include_parents: true.
	//
	// For 24K Core the entry's ExternalRecordID becomes
	// asset_classification.parent_id. With include_parents: false the anchor's
	// types are pushed as roots and this key is simply absent — the flat-catalog
	// case, not an error.
	//
	// It cannot collide with an entity-lane edge key even though "parent_type" is
	// a legal identifier a kit could name an edge: the two lanes never share a
	// batch, so the two key spaces never meet inside one payload.
	OntologyParentRefKey = "parent_type"
)

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

// SyncRequest is the body of a push — POST /egress/sync for entities, and
// POST /egress/ontology-sync for ontology type records. One type serves both
// because the two carry the same fields; what differs is the KIND of record in
// Entities, and that is carried by the ROUTE, never by a field here. See
// [OntologyTypeSyncer].
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
	// manifest's entities_sync[] entry it corresponds to is the same string.
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
	//
	// LEGACY, and retained only for that: it is what a platform predating
	// ReasonDetail reads. [Batch.Failed] still populates it, so a component needs
	// no change. A SKIPPED result never sets it — a consumer keying on
	// `error != ""` must never see a success dressed as a failure.
	Error string `json:"error,omitempty"`

	// ReasonCode is the STABLE, machine-readable classification of why this
	// record was skipped or failed. Valid on `skipped` and `failed` alike; never
	// set on created/updated, which need no reason.
	//
	// This is the field the operator's run report GROUPS BY, which is the whole
	// reason it exists separately from the prose: a report that tallied a
	// free-text message would key on wording, so re-phrasing a message would
	// silently split one cause into two and an operator could not tell whether
	// one cause dominated a step. Prefer a small closed vocabulary the component
	// reuses (`predicate_unmapped`, `entity_type_excluded`) over a per-record
	// string.
	//
	// Empty is a legitimate, honest answer — see [Batch.Skipped]. The platform
	// tallies an unattributed verdict under a code of its own that says exactly
	// that, rather than inventing a cause.
	//
	// Two prefixes are RESERVED and a component's code may not use them:
	// "platform:" for codes the platform mints for its own verdicts, and "sdk:"
	// for the ones this package mints (see the ReasonCode* constants). A code
	// carrying either is dropped with a defect rather than forwarded, so a
	// component can never make its own classification read as the platform's.
	ReasonCode string `json:"reason_code,omitempty"`

	// ReasonDetail is the human prose behind ReasonCode — the specifics a code
	// cannot carry, such as WHICH field was missing or WHICH predicate had no
	// mapping. Valid on `skipped` and `failed` alike.
	//
	// On a failure it carries the same text as Error; the duplication is
	// deliberate and cheap, and it is what lets a kit author treat
	// (ReasonCode, ReasonDetail) as meaning one thing on both verdicts instead of
	// remembering that one of them reports its prose in a field called "error".
	ReasonDetail string `json:"reason_detail,omitempty"`
}

// Reason codes this package mints, for the three component bugs
// [Batch.Results] normalizes into a per-record failure rather than propagating.
//
// They exist so the operator's report can group them: before them, all three
// arrived as distinct prose sentences and a run whose component returned no
// verdict for 300 records reported 300 individually-worded failures with no
// indication they shared one cause.
//
// The "sdk:" prefix is reserved — a component's own code may not use it, so a
// code in this namespace is always one this package assigned.
const (
	// ReasonCodeNoVerdict: the component recorded nothing for a requested id.
	ReasonCodeNoVerdict = "sdk:no_verdict"
	// ReasonCodeUnrecognizedOutcome: the outcome was not one of the four.
	ReasonCodeUnrecognizedOutcome = "sdk:unrecognized_outcome"
	// ReasonCodeMissingExternalRecordID: created/updated with no external id.
	ReasonCodeMissingExternalRecordID = "sdk:missing_external_record_id"
	// ReasonCodeNoReason: the component failed a record without saying why.
	ReasonCodeNoReason = "sdk:no_reason"
)

// reservedReasonPrefixes may not appear on a component-supplied reason code.
//
// "platform:" is the platform's own namespace and "sdk:" is this package's. A
// component that could write either would be able to make its own guess read as
// an authoritative classification in the run report.
var reservedReasonPrefixes = []string{"platform:", "sdk:"}

// maxReasonCodeLen bounds a reason code.
//
// A code is a grouping key that the platform carries in a bounded per-step tally
// across every workflow continuation, so an unbounded one would put arbitrary
// kit-authored bytes into that payload. 64 is far more than a real vocabulary
// needs; anything longer is prose that belongs in the detail.
const maxReasonCodeLen = 64

// normalizeReasonCode validates a component-supplied reason code, returning the
// code to record and a defect description when it was refused.
//
// A refused code is DROPPED rather than replaced: the verdict itself is still
// recorded, and the platform then reports the record as unattributed — which is
// true — instead of carrying a classification the component is not entitled to.
func normalizeReasonCode(code string) (string, string) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return "", ""
	}
	if len(trimmed) > maxReasonCodeLen {
		return "", fmt.Sprintf("reason code %q exceeds %d bytes and was dropped; "+
			"a code is a grouping key, so put the specifics in the detail", trimmed, maxReasonCodeLen)
	}
	if strings.ContainsFunc(trimmed, unicode.IsSpace) {
		return "", fmt.Sprintf("reason code %q contains whitespace and was dropped; "+
			"a code is a grouping key, so put the specifics in the detail", trimmed)
	}
	for _, p := range reservedReasonPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return "", fmt.Sprintf("reason code %q uses the reserved %q prefix and was dropped; "+
				"that namespace belongs to the platform, not to a component", trimmed, p)
		}
	}
	return trimmed, ""
}

// SyncResponse is the body of a push reply, on either lane.
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
