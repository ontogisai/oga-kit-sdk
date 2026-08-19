package manifest

import (
	"fmt"
	"strings"
)

// Egress-sync manifest declarations (kg-egress-sync, OGA-775 / OGA-810).
//
// An egress component is the OUTBOUND counterpart of a Source Connector, and
// the shapes here deliberately mirror SourceConnectorSpec so a kit author moving
// between the two directions is not learning a second convention.
//
// The division of responsibility is what these fields encode. The platform
// reads only four things from the declaration: which entity types, in what
// order, how to batch them, and which string to record as external_system.
// Everything domain-specific — the external system's DTOs, its auth, its field
// derivations — lives in the component's own code behind the egress HTTP
// contract (see the sibling egress package).
//
// These types mirror the platform's internal/domainkit.EgressSyncSpec /
// EgressEntityTypeSpec field-for-field so YAML authored against either parses
// identically under both, and a kit author gets the platform installer's
// field-level errors locally instead of at install time.

// Default batching knobs, mirroring the platform's domainkit defaults. Modest
// on purpose: the scarce resource in an egress run is the EXTERNAL system, not
// the platform.
const (
	// DefaultEgressBatchSize is the number of entities per POST /egress/sync
	// when the kit declares none.
	DefaultEgressBatchSize = 200
)

// EgressSyncSpec declares one egress-sync component the platform deploys as a
// long-running sidecar at install time and then DRIVES: a resumable Day-1 bulk
// push of the declared entity types plus continuous Day-2 change sync.
type EgressSyncSpec struct {
	// Name is the component instance name (unique within the kit). It forms the
	// sidecar registry name {tenant}.{name} and is what an operator passes to
	// `oga-admin egress sync --component`.
	Name string `yaml:"name"`

	// ExternalSystem is the system-of-record key — the value the platform
	// writes to each pushed entity's external_system column. This declaration
	// is the ONLY place that string is defined, so the correlation the platform
	// records and the system the component pushes to cannot drift apart. It is
	// never defaultable.
	ExternalSystem string `yaml:"external_system"`

	// EntityTypes are the types to push, in the order they must be pushed.
	//
	// The order is load-bearing and the platform honors it without interpreting
	// it: a type is fully pushed and correlated before the next begins, so a
	// later type may reference an earlier one in the external system. The
	// platform does not know WHY the order matters — only that the kit declared
	// it.
	EntityTypes []EgressEntityTypeSpec `yaml:"entity_types"`

	// CredentialRefs lists SecretStore secret names the platform delivers to
	// the component so it can authenticate to the external system. Resolved per
	// tenant by the same path source connectors use.
	CredentialRefs []string `yaml:"credential_refs,omitempty"`

	// BatchSize is the number of entities per POST /egress/sync call.
	//
	// Deliberately separate from the platform's internal read page size: the
	// binding constraint here is the EXTERNAL API's array limit, not how
	// efficiently the platform can page its own store. Zero ⇒
	// DefaultEgressBatchSize.
	BatchSize int `yaml:"batch_size,omitempty"`

	// MaxInFlight is the number of concurrent batches for a type.
	//
	// IGNORED for any type declaring ParentEdge: concurrent batches within a
	// hierarchical type would let a child be pushed before its parent, which is
	// exactly the ordering the level-by-level walk exists to guarantee. Zero or
	// less ⇒ 1 (sequential). Use EffectiveMaxInFlight to resolve.
	MaxInFlight int `yaml:"max_in_flight,omitempty"`

	// Image is the container image reference (with digest). DEPRECATED as the
	// declaration site: declare it under container.image instead, consistent
	// with agents / MCP servers / loaders / source connectors (OGA-637). Still
	// honored as a fallback; when both are set container.image wins. Use
	// EffectiveImage() to resolve.
	Image string `yaml:"image,omitempty"`

	// Container holds the component's deployment details. container.image is
	// the CANONICAL place to declare the image; the block also carries port,
	// resources, and env. An env value of the form "secret://<name>" is
	// resolved per tenant from the SecretStore at container start, which is how
	// a kit delivers non-baked config such as the external system's base URL.
	Container SidecarContainerSpec `yaml:"container,omitempty"`
}

// EgressEntityTypeSpec is one entity type an egress component pushes.
type EgressEntityTypeSpec struct {
	// Name is the entity type to push. It must exist in the tenant's active
	// ontology at install time — a check the platform performs and the SDK
	// deliberately does not, since it needs tenant state no local lint can see.
	Name string `yaml:"name"`

	// ParentEdge names the SELF-REFERENCING containment edge of this type, when
	// it has one. Its presence changes the platform's walk from "page by id" to
	// "level by level, roots first".
	//
	// Declare it whenever a record of this type references its parent in the
	// external system. A page-by-id walk yields ARBITRARY order, so without
	// this a child can be pushed before its parent and that push fails.
	//
	// The platform treats this as an edge NAME and nothing more: it never
	// interprets what containment means, only "shallower before deeper".
	ParentEdge string `yaml:"parent_edge,omitempty"`
}

// EffectiveImage returns the component's container image, preferring the
// canonical Container.Image and falling back to the deprecated top-level Image
// (OGA-637). Every reader of an egress image — validation here, and the
// platform's deploy, upgrade image-replace and image-trust paths — resolves
// through the equivalent helper, so they cannot disagree about where the image
// lives.
func (e *EgressSyncSpec) EffectiveImage() string {
	if e.Container.Image != "" {
		return e.Container.Image
	}
	return e.Image
}

// EffectiveBatchSize returns the push batch size, defaulted.
func (e *EgressSyncSpec) EffectiveBatchSize() int {
	if e.BatchSize > 0 {
		return e.BatchSize
	}
	return DefaultEgressBatchSize
}

// EffectiveMaxInFlight returns the concurrency for entityType.
//
// It returns 1 unconditionally for a type declaring ParentEdge, whatever the
// kit asked for. This is not a kit-authoring error worth failing an install
// over — the declared concurrency is simply not applicable to a hierarchical
// type — but honoring it would silently break ancestor-before-descendant
// ordering, so it is clamped here rather than checked at every call site.
func (e *EgressSyncSpec) EffectiveMaxInFlight(entityType *EgressEntityTypeSpec) int {
	if entityType != nil && entityType.ParentEdge != "" {
		return 1
	}
	if e.MaxInFlight > 1 {
		return e.MaxInFlight
	}
	return 1
}

// EntityTypeNames returns the declared entity type names in push order,
// skipping empty entries.
func (e *EgressSyncSpec) EntityTypeNames() []string {
	if len(e.EntityTypes) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.EntityTypes))
	for i := range e.EntityTypes {
		if n := e.EntityTypes[i].Name; n != "" {
			out = append(out, n)
		}
	}
	return out
}

// validateEgressSyncs checks each kit-declared egress component
// (spec.egress_syncs[]): unique name, external_system present, at least one
// entity type, and a digest-pinned image. It mirrors the platform's
// domainkit.validateEgressSpecStructure — the checks that need no external
// state — so a kit author gets the same rejection locally that the installer
// would raise as OGA-EGRS-VAL-1002.
//
// It deliberately does NOT mirror the platform's ontology cross-check
// (OGA-EGRS-VAL-1001: every declared entity type must resolve against the
// tenant's active ontology). That needs per-tenant state the SDK cannot see, so
// it stays install-time only — and a local lint that silently skipped it would
// be worse than one that never claimed to do it.
func validateEgressSyncs(syncs []EgressSyncSpec) error {
	if len(syncs) == 0 {
		return nil
	}
	seen := make(map[string]int, len(syncs))
	for i := range syncs {
		e := &syncs[i]
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("spec.egress_syncs[%d]: name is required", i)
		}
		if prev, ok := seen[e.Name]; ok {
			return fmt.Errorf(
				"spec.egress_syncs[%d]: name = %q duplicates spec.egress_syncs[%d]",
				i, e.Name, prev,
			)
		}
		seen[e.Name] = i
		// external_system cannot be defaulted: it is the value the platform
		// records as each pushed entity's system of record, and a wrong or empty
		// value makes the correlation unresolvable on the next update.
		if strings.TrimSpace(e.ExternalSystem) == "" {
			return fmt.Errorf("spec.egress_syncs[%d]: external_system is required", i)
		}
		if len(e.EntityTypeNames()) == 0 {
			return fmt.Errorf("spec.egress_syncs[%d]: at least one entity type is required", i)
		}
		image := e.EffectiveImage()
		if image == "" {
			return fmt.Errorf("spec.egress_syncs[%d]: image is required (declare it under container.image)", i)
		}
		// Digest-pinning is required for the same reason it is for every other
		// kit sidecar: a mutable tag means the running component is not the one
		// that was signature-verified at upload.
		if !strings.Contains(image, "@sha256:") {
			return fmt.Errorf(
				"spec.egress_syncs[%d]: image must be digest-pinned (image@sha256:...), got %q",
				i, image,
			)
		}
	}
	return nil
}
