package transfer

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/language"
)

// Limits and identifiers for the on-disk transfer format. These are part
// of the kit-platform contract — changing them requires a coordinated
// SDK + platform release.
const (
	// InlineBodyLimit is the maximum encoded payload size that may be
	// committed inline through loader.complete. Above this size, the
	// writer switches to a presigned-URL upload path.
	//
	// 700 KiB is the threshold. The MCP server's request body limit is
	// 1 MiB and the JSON-RPC inline_body field is base64-encoded
	// (~1.333× expansion), so a raw NDJSON body up to 700 KiB stays
	// safely under the wire limit (~933 KiB encoded plus envelope).
	// Larger artifacts (campus-scale data, IFC imports, ontologies with
	// many type defs) use the presigned upload path which streams
	// directly to object storage without buffering in the gateway or
	// MCP server.
	InlineBodyLimit = 700 << 10 // 700 KiB

	// MultiPassThreshold is the source-file size above which kit
	// authors should adopt the multi-pass loader.StreamingLoaderHandler
	// pattern to keep the parser's working set bounded. Below this
	// threshold a single-pass LoaderHandler is fine.
	MultiPassThreshold = 5 << 20 // 5 MiB

	// FormatVersion is written in the artifact header line. Bump on
	// breaking changes to the on-disk schema.
	FormatVersion = 1

	// FormatNDJSON is the only supported on-disk format today.
	// Newline-delimited JSON: one record per line, header line first.
	FormatNDJSON = "ndjson"
)

// LoadKind identifies the platform-side processor that should consume
// the artifact. Set automatically by the writer based on which
// constructor the kit used (NewOntologyWriter vs NewDataWriter); kit
// authors do not pick this value directly.
type LoadKind string

const (
	// KindOntology asks the platform to register the artifact as a
	// batch of entity types: atomic DDL + EntityTypeDef + activation,
	// matching the kit-installer YAML path.
	KindOntology LoadKind = "ontology"

	// KindData asks the platform to persist vertices and edges into
	// the active schema for the tenant. Embedding generation kicks in
	// via the existing ingestion.resolved.* event subscriber.
	KindData LoadKind = "data"
)

// Receipt is what [Writer.Close] returns. The kit usually only cares
// about JobID (so it can include it in the LoadResponse stats); the
// other fields are useful for tests and observability.
type Receipt struct {
	// JobID is the platform-issued identifier the install / import
	// workflow polls via loader.status until terminal.
	JobID string

	// ContentHash is the sha256 of the encoded artifact body that the
	// platform validated. The same (tenant, kit, content_hash) tuple
	// is idempotent — re-running a loader with unchanged input is a
	// no-op on the platform side.
	ContentHash string

	// BytesWritten is the size of the encoded artifact (whether it
	// went inline or through a presigned upload).
	BytesWritten int64

	// EntryCount is the number of records in the artifact (vertices +
	// edges + entity-type defs + hierarchy entries combined).
	EntryCount int

	// Mode tells the kit which transport the writer used. "inline"
	// means the body fit under InlineBodyLimit and was sent as a
	// loader.complete request body; "presigned" means the writer
	// streamed to a presigned PUT URL and only the upload_token went
	// to loader.complete. Useful for tests; kits should not branch on
	// this.
	Mode TransportMode

	// AcceptedAt is the platform time the commit was accepted.
	AcceptedAt time.Time
}

// TransportMode reports which transport [Writer] used for a given
// load. Set on [Receipt] for observability.
type TransportMode string

const (
	// TransportInline — committed via loader.complete inline body.
	TransportInline TransportMode = "inline"

	// TransportPresigned — uploaded to a presigned PUT URL, then
	// committed by upload_token reference.
	TransportPresigned TransportMode = "presigned"
)

// Vertex is the entity instance shape carried over the wire. Fields
// mirror the platform's BaseEntity inheritance: only domain-specific
// data lives here; tenant_id, audit fields, bi-temporal fields, and H3
// indices are added by the platform persister.
type Vertex struct {
	// ID is a stable, deterministic identifier the kit chose for this
	// entity. May be empty — when blank, the platform derives one
	// from EntityType + Properties.
	ID string `json:"id,omitempty"`

	// EntityType is the type name (must match a registered type, e.g.,
	// "brick_Equipment", "WorkOrder").
	EntityType string `json:"entity_type"`

	// Label is a human-readable label.
	Label string `json:"label,omitempty"`

	// Properties are the domain-specific property values.
	Properties map[string]any `json:"properties,omitempty"`

	// Latitude / Longitude — when both are present, the platform
	// computes H3 indices at the configured resolutions.
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`

	// CorrelationKey, when set, asks the platform to resolve this vertex to
	// an EXISTING entity by its external reference (external_system +
	// external_record_id) instead of by ID — the close-the-loop / status-sync
	// path for continuous Source Connectors. When set with an empty ID the
	// platform performs external-ref resolution: a match merges onto the
	// existing entity (and updates its ExternalSystemRecord); no match is
	// quarantined for tenant review (never a phantom entity), unless the
	// source opts into create-on-miss.
	//
	// Leave nil for ordinary create/update, where the platform UPSERTs by
	// (id, tenant_id) as before. Additive and back-compatible: loaders and
	// existing kits that never set it are unaffected.
	CorrelationKey *CorrelationKey `json:"correlation_key,omitempty"`
}

// CorrelationKey is the external reference an inbound record carries so the
// platform can locate the existing KG entity it corresponds to. It is the
// search key persisted on hybrid domain entities and on ExternalSystemRecord
// by the outbound action executor, so an inbound status update can find the
// same vertex without knowing its platform ID.
type CorrelationKey struct {
	// ExternalSystem names the system of record the external_record_id
	// belongs to (e.g. "contract_wo_mgmt", "sap"). Matches
	// ExternalSystemRecord.external_system.
	ExternalSystem string `json:"external_system"`

	// ExternalRecordID is the identifier the external system assigned to the
	// record (e.g. a work-order number). Matches
	// ExternalSystemRecord.external_record_id.
	ExternalRecordID string `json:"external_record_id"`
}

// Edge is the relationship instance shape carried over the wire.
type Edge struct {
	// ID is a stable identifier; empty lets the platform derive one
	// from source + target + type.
	ID string `json:"id,omitempty"`

	// RelationshipType is the edge type name (e.g., "hasLocation").
	RelationshipType string `json:"relationship_type"`

	// SourceID is the ID of the source vertex.
	SourceID string `json:"source_id"`

	// TargetID is the ID of the target vertex.
	TargetID string `json:"target_id"`

	// Properties are optional edge properties.
	Properties map[string]any `json:"properties,omitempty"`
}

// Materialization controls whether an ontology type is a real DDL
// vertex/edge type (physical) or a catalog-only logical type stored
// under a coarser physical type (hybrid ontology modeling, OGA-584).
// The empty value reads back as physical, so pre-hybrid loaders and
// typed kits that never set it are unaffected.
type Materialization = string

// Recognized materialization values.
const (
	// MaterializationPhysical is a real DDL vertex/edge type. Default —
	// an empty Materialization is treated as physical.
	MaterializationPhysical Materialization = "physical"

	// MaterializationLogical is a catalog-only type: the platform registers
	// its EntityTypeDef/RelationshipTypeDef row (for validate / describe /
	// semantic search) but emits NO DDL. Instances are stored under the
	// coarser PhysicalType. Requires a non-empty PhysicalType.
	MaterializationLogical Materialization = "logical"
)

// EntityTypeDef is the shape used by ontology loaders to register an
// entity type. The platform's ontology dispatcher creates the DDL
// (CREATE VERTEX TYPE … EXTENDS BaseEntity), inserts an EntityTypeDef
// row, and activates the resulting ontology version atomically.
type EntityTypeDef struct {
	// Name is the stable identifier (e.g., "brick_Equipment"). Must
	// match the DDL type name without tenant prefix; the platform
	// adds the prefix during persistence.
	Name string `json:"name"`

	// DisplayName is the human-readable name keyed by full BCP-47
	// locale tag (e.g., "en-US", "vi-VN"). Short-form keys ("en",
	// "vi") are rejected by ValidateLocaleKeys — kit authors must
	// be explicit about the region so the platform's locale parser
	// cannot silently disagree on the intended tag.
	// Example: {"en-US": "Equipment (Brick)", "vi-VN": "Thiết Bị (Brick)"}.
	DisplayName map[string]string `json:"display_name,omitempty"`

	// Description is a detailed description keyed by full BCP-47
	// locale tag — same convention as DisplayName. The en-US entry
	// is the canonical input for embedding generation on the
	// platform side.
	Description map[string]string `json:"description,omitempty"`

	// ParentType is the parent type's Name. Empty means this type is
	// at the root of the kit's hierarchy.
	ParentType string `json:"parent_type,omitempty"`

	// Category classifies the type ("equipment", "location", ...).
	Category string `json:"category,omitempty"`

	// Properties lists domain-specific properties. Used for embedding
	// text generation; not a substitute for DDL property definitions
	// (those flow through the same writer via separate WriteProperty
	// calls when the kit needs them — out of scope for v1).
	Properties []TypeProperty `json:"properties,omitempty"`

	// Materialization is "physical" (default/empty) or "logical" — the
	// hybrid ontology modeling flag (OGA-584). A logical type is registered
	// in the catalog with NO DDL and its instances are stored under
	// PhysicalType. Empty ⇒ physical, so pre-hybrid loaders are unaffected.
	Materialization Materialization `json:"materialization,omitempty"`

	// PhysicalType is the materialised physical type a logical type's
	// instances are stored under. Required iff Materialization == logical;
	// ignored for physical types. It must name a physical type that is an
	// ancestor of this type in the kit's hierarchy — the platform
	// coherence-validates this at registration time.
	PhysicalType string `json:"physical_type,omitempty"`
}

// RelationshipTypeDef is the ontology relationship (edge) type contract. It
// mirrors the relationship_types entries a kit declares in its ontology YAML
// (name, display_name, description, source_type, target_type, cardinality,
// properties) and carries the same hybrid ontology modeling fields as
// EntityTypeDef (OGA-584, C7). A physical relationship type is a real DDL
// edge type (EXTENDS BaseRelationship); a logical one is a catalog-only
// predicate mapped onto a coarser physical edge type (e.g. the generic
// RELATES), with the fine predicate carried in relationship_type data.
//
// Two paths register relationship types dynamically (OGA-659):
//
//   - The transfer writer, via [Writer.WriteRelationshipType], lets a
//     kind=ontology loader stream edge-type definitions alongside its entity
//     types + hierarchy — the streaming-loader edge path, symmetric with
//     WriteEntityType. The platform MERGES them into the active ontology (it
//     never carries relationship types forward blindly — OGA-564).
//   - The connector [OntologySnapshot] carries the full desired relationship
//     type set for a Source Connector's gated ontology-refresh.
//
// New predicates a loader/connector does not register still resolve to the
// generic RELATES edge type at edge-write time — the safety net for
// genuinely-unknown predicates.
type RelationshipTypeDef struct {
	// Name is the stable identifier (e.g. "equipmentHasSchedule"). Must
	// match the DDL type name without tenant prefix; the platform adds the
	// prefix during persistence.
	Name string `json:"name"`

	// DisplayName is the human-readable name keyed by full BCP-47 locale
	// tag (e.g. "en-US", "vi-VN"). Short-form keys are rejected by
	// ValidateLocaleKeys — same convention as EntityTypeDef.
	//
	// The platform stores these per-locale as the relationship type's
	// localized label (its NameI18n), rendered per-locale at the API boundary
	// (OGA-659). Unlike EntityTypeDef there is no en-US "display name" scalar
	// on the platform edge — an edge has no keyword-search surface that would
	// consume one — but every locale you set here is preserved.
	DisplayName map[string]string `json:"display_name,omitempty"`

	// Description is a detailed description keyed by full BCP-47 locale tag.
	// The en-US entry is the canonical input for the relationship type's
	// embedding text (semantic predicate search); all locales are also
	// preserved as the platform's DescriptionI18n (OGA-659).
	Description map[string]string `json:"description,omitempty"`

	// SourceType is the source entity type name, or "*" for any. Mirrors
	// the ontology YAML source_type field.
	SourceType string `json:"source_type,omitempty"`

	// TargetType is the target entity type name, or "*" for any. Mirrors
	// the ontology YAML target_type field.
	TargetType string `json:"target_type,omitempty"`

	// Cardinality is the relationship cardinality (e.g. "one_to_many",
	// "many_to_one", "many_to_many"). Free-form to the SDK; the platform
	// validates against its recognized set.
	Cardinality string `json:"cardinality,omitempty"`

	// Properties lists domain-specific edge properties.
	Properties []TypeProperty `json:"properties,omitempty"`

	// Materialization is "physical" (default/empty) or "logical" — same
	// semantics as EntityTypeDef.Materialization (OGA-584, C7). Empty ⇒
	// physical.
	Materialization Materialization `json:"materialization,omitempty"`

	// PhysicalType is the materialised physical edge type a logical
	// relationship's instances are stored under (e.g. "RELATES"). Required
	// iff Materialization == logical.
	PhysicalType string `json:"physical_type,omitempty"`
}

// TypeProperty describes a property on an EntityTypeDef.
type TypeProperty struct {
	Name string `json:"name"`
	// Description is keyed by full BCP-47 locale tag (e.g. "en-US",
	// "vi-VN"). Short-form keys ("en", "vi") are rejected by
	// ValidateLocaleKeys.
	Description map[string]string `json:"description,omitempty"`
	Type        string            `json:"type,omitempty"`
	Required    bool              `json:"required,omitempty"`

	// NOTE (KIV): there is deliberately NO allowed-values / enum field here
	// yet. The platform's ontology model keeps a per-property allowed-values
	// list (domainkit PropertyDef.Values, YAML `values:`) reserved for a
	// planned enum-constraint feature, but it is not yet enforced/persisted
	// and is NOT part of this wire contract. When enum support lands, add a
	// `Values []string json:"values,omitempty"` here (and to the ontology
	// Registrar) so an enum declared through the programmatic loader path can
	// flow to the platform — until then it cannot. Tracked in OGA-615.
}

// HierarchyEntry declares a parent-child relationship between two
// types. The platform uses this to materialize the type-inheritance
// graph; it is also encoded on each EntityTypeDef.ParentType, but
// shipping the explicit hierarchy lets the platform validate the
// graph (no cycles, no missing parents) before activating.
type HierarchyEntry struct {
	TypeName   string `json:"type_name"`
	ParentType string `json:"parent_type"`
}

// Header is the first line of every artifact. The platform-side
// reader validates Format and FormatVersion before consuming.
type Header struct {
	// Format is "ndjson" — the only supported on-disk format today.
	Format string `json:"format"`

	// FormatVersion is the schema version of the artifact body.
	FormatVersion int `json:"format_version"`

	// Kind tells the platform which dispatcher should consume the
	// artifact. Always set by the writer; kit authors don't choose.
	Kind LoadKind `json:"kind"`

	// KitID is informational; tenant_id and the authoritative kit
	// identity come from the gateway auth context.
	KitID string `json:"kit_id,omitempty"`

	// EdgeCompleteness is the connector's assertion about how complete
	// the edge set in this artifact is (OGA-914).
	//
	// DEPRECATED (OGA-930), and already INERT against a current platform:
	// the platform no longer reads it. It logs a warning if present and
	// resolves the assertion from its OWN state instead, keyed on the
	// gateway-verified submitter identity — so the claim never travels
	// through the artifact and a kit image built before the feature still
	// converges.
	//
	// Declare completeness in the kit MANIFEST instead:
	//
	//	source_connectors:
	//	  - name: asset-sync
	//	    edge_completeness:
	//	      mode: per_source
	//	      governed_predicates_file: predicates/forward.json
	//
	// A data loader driven by an operator import needs nothing here at
	// all: the operator asserts per run (the full_snapshot flag on the
	// import), and the vocabulary comes from the manifest's
	// loaders[].governed_predicates_file.
	//
	// Kept for one release so a kit still setting it compiles. Setting it
	// is harmless but achieves nothing. Removal tracked with the platform
	// cut.
	//
	// Deprecated: declare edge completeness in the kit manifest.
	EdgeCompleteness *EdgeCompleteness `json:"edge_completeness,omitempty"`
}

// EdgeCompletenessMode names how complete the edge set in an artifact
// is. The zero value is the pre-existing behaviour, so a kit that
// never sets it is unaffected.
type EdgeCompletenessMode string

const (
	// EdgeCompletenessPartial (the zero value) — the artifact's edges
	// are a partial batch. Absence of an edge asserts NOTHING, so the
	// platform closes nothing. Every loader, NGSI-LD feed and MCP
	// caller is in this mode.
	EdgeCompletenessPartial EdgeCompletenessMode = ""

	// EdgeCompletenessPerSource — for EVERY vertex this artifact
	// carries, the edges it carries FROM that vertex under
	// GovernedPredicates are the COMPLETE set, INCLUDING the empty
	// set. The platform closes a live edge whose source is one of
	// those vertices, whose predicate is governed, and which the
	// artifact does not carry.
	//
	// ⚠️ Two obligations come with this mode, and the platform can
	// verify neither:
	//
	//  1. COLLAPSE INVERSE PAIRS FIRST. If the upstream declares a
	//     relationship from both ends (`feeds` on the damper,
	//     `isFedBy` on the AHU), those are ONE canonical edge and the
	//     connector must emit it under one canonical source whichever
	//     end declared it. The platform honours the submitted set
	//     verbatim and never re-derives an owner, so a connector that
	//     asserts this mode without collapsing WILL close edges the
	//     other endpoint still declares.
	//  2. ONE ARTIFACT PER SNAPSHOT. A source's edges must be in the
	//     SAME artifact as its vertex. Splitting one logical export
	//     across two artifacts makes each one's vertex set partial,
	//     and the first would close the edges the second carries.
	EdgeCompletenessPerSource EdgeCompletenessMode = "per_source"
)

// EdgeCompleteness is the assertion carried on the artifact header.
type EdgeCompleteness struct {
	// Mode is how complete the edge set is. See the mode constants.
	Mode EdgeCompletenessMode `json:"mode"`

	// GovernedPredicates bounds which relationship types the assertion
	// covers — the feed's own vocabulary. A live edge under a
	// predicate NOT listed here is never closed, however complete the
	// source's other edges are.
	//
	// Required (non-empty) in EdgeCompletenessPerSource. An empty list
	// is REFUSED rather than read as "governs everything": a
	// connector's export routinely shares a source with edges written
	// by another feed, the MCP tools or the platform itself, and
	// closing those is not a mistake anything downstream could
	// review. Values are the source-native predicate names, verbatim.
	GovernedPredicates []string `json:"governed_predicates,omitempty"`
}

// Validate reports whether the assertion is well formed. A nil
// receiver is valid (no assertion).
func (e *EdgeCompleteness) Validate() error {
	if e == nil {
		return nil
	}
	switch e.Mode {
	case EdgeCompletenessPartial:
		// No assertion. Governed predicates are meaningless here, and
		// silently accepting them would let a kit believe it had opted
		// in when it had only named a vocabulary.
		if len(e.GovernedPredicates) > 0 {
			return errors.New("transfer: edge_completeness.governed_predicates set without a mode; " +
				"set mode to \"per_source\" to assert completeness")
		}
		return nil
	case EdgeCompletenessPerSource:
		for _, p := range e.GovernedPredicates {
			if strings.TrimSpace(p) == "" {
				return errors.New("transfer: edge_completeness.governed_predicates contains an empty predicate")
			}
		}
		if len(e.GovernedPredicates) == 0 {
			return fmt.Errorf("transfer: edge_completeness mode %q requires at least one governed_predicate",
				EdgeCompletenessPerSource)
		}
		return nil
	default:
		return fmt.Errorf("transfer: unsupported edge_completeness mode %q (supported: %q)",
			e.Mode, EdgeCompletenessPerSource)
	}
}

// Asserted reports whether the header carries an assertion the
// platform should act on. Nil-safe.
func (e *EdgeCompleteness) Asserted() bool {
	return e != nil && e.Mode == EdgeCompletenessPerSource
}

// EntryKind classifies a single record in the body. Each line after
// the header is an envelope `{"kind": "...", "value": <record>}` so
// the platform reader can stream-decode without holding the whole
// artifact in memory.
type EntryKind string

const (
	EntryVertex           EntryKind = "vertex"
	EntryEdge             EntryKind = "edge"
	EntryEntityType       EntryKind = "entity_type"
	EntryRelationshipType EntryKind = "relationship_type"
	EntryHierarchy        EntryKind = "hierarchy"
)

// Envelope wraps each non-header record so the platform's stream
// reader can dispatch to the right decoder by Kind.
type Envelope struct {
	Kind  EntryKind `json:"kind"`
	Value any       `json:"value"`
}

// ValidateLocaleKeys reports whether every key in m is a valid full
// BCP-47 language tag (e.g., "en-US", "vi-VN", "zh-CN"). Short-form
// language-only tags ("en", "vi") are rejected — kit code that
// constructs EntityTypeDef.DisplayName / .Description /
// TypeProperty.Description maps must use the full form so the
// platform's locale parser cannot silently disagree on which tag the
// kit means. The fieldName argument prefixes any error returned so
// the kit author can find the offending map quickly. Empty or nil
// maps are always valid.
//
// This mirrors manifest.ValidateLocaleKeys and lives in the transfer
// package as a convenience for ontology loaders that don't import
// the manifest package.
func ValidateLocaleKeys(fieldName string, m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			return fmt.Errorf(
				"%s: locale key is empty (use full BCP-47 like \"en-US\", \"vi-VN\")",
				fieldName,
			)
		}
		if _, err := language.Parse(k); err != nil {
			return fmt.Errorf(
				"%s: locale key %q is not a valid BCP-47 tag: %w",
				fieldName, k, err,
			)
		}
		// See manifest.validateLocaleKeys for the rationale on why
		// short-form tags are rejected even when the language parser
		// would happily infer a likely region.
		if !strings.Contains(k, "-") {
			return fmt.Errorf(
				"%s: locale key %q must be a full BCP-47 tag with a region "+
					"(e.g., %q-US, %q-GB) — short-form language-only tags are rejected",
				fieldName, k, k, k,
			)
		}
	}
	return nil
}
