package transfer

import (
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
	// Larger artifacts (SJ campus data, IFC imports, ontologies with
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
}

// EntryKind classifies a single record in the body. Each line after
// the header is an envelope `{"kind": "...", "value": <record>}` so
// the platform reader can stream-decode without holding the whole
// artifact in memory.
type EntryKind string

const (
	EntryVertex     EntryKind = "vertex"
	EntryEdge       EntryKind = "edge"
	EntryEntityType EntryKind = "entity_type"
	EntryHierarchy  EntryKind = "hierarchy"
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
