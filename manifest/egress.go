package manifest

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
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

// ParentEdgeDirection is the direction in which an owner edge is traversed to
// reach the owner.
//
// The values match ArcadeDB's own out() / in() traversal vocabulary rather than
// spelling them "outbound" / "inbound", so a declaration reads directly onto the
// traversal the platform generates for it.
type ParentEdgeDirection string

const (
	// ParentEdgeOut resolves the owner as out(edge): the declaring entity holds
	// the edge and it points AT its owner. This is the default, and it is correct
	// whenever a containment edge is stored child to parent.
	ParentEdgeOut ParentEdgeDirection = "out"

	// ParentEdgeIn resolves the owner as in(edge): the OWNER holds the edge and it
	// points at the declaring entity. Needed whenever a containment edge is stored
	// parent to child, which is the usual convention for "has a part / has a
	// point" style predicates.
	ParentEdgeIn ParentEdgeDirection = "in"
)

// NormalizeDirection resolves an unset direction to the outbound default.
//
// Outbound is the default because it is both the more common storage convention
// for containment and the behaviour every declaration had before direction
// existed, so an omitted value means what it always meant.
func NormalizeDirection(d ParentEdgeDirection) ParentEdgeDirection {
	if d == "" {
		return ParentEdgeOut
	}
	return d
}

// Valid reports whether d is a direction the platform can traverse. An empty
// value is valid: it normalizes to the outbound default.
func (d ParentEdgeDirection) Valid() bool {
	switch NormalizeDirection(d) {
	case ParentEdgeOut, ParentEdgeIn:
		return true
	default:
		return false
	}
}

// ParentEdgeSpec is one owner-edge declaration: an edge name plus the direction
// in which the platform traverses it to reach the owner.
//
// YAML accepts two forms, and the scalar shorthand is what keeps the common case
// unchanged:
//
//	parent_edges: [hasLocation]                        # shorthand ⇒ direction: out
//	parent_edges:
//	  - edge: hasPoint                                 # explicit
//	    direction: in
//
// Direction is a property of the (declaring type, edge) PAIR, not of the edge, for
// the same reason Hierarchical is: one predicate is oriented differently by
// different declaring types. A campus kit's hasLocation runs child to parent while
// its hasPoint runs parent to child, so no attribute of the edge alone could carry
// it.
//
// Encoding direction as a sigil inside the edge string ("<hasPoint") was rejected:
// isAddressableEdgeName permits [a-zA-Z0-9_] only, so every sigil form is already
// rejected as unaddressable, and the sigil would leak into the parent_refs key the
// component reads.
type ParentEdgeSpec struct {
	// Edge is the relationship type name. It must ALREADY be a legal ArcadeDB
	// local identifier — see the note on EgressEntityTypeSpec.ParentEdges, where
	// the colon asymmetry against Name is spelled out.
	Edge string `yaml:"edge"`

	// Direction is "out" (default) or "in". Empty means out; use
	// EffectiveDirection to read it resolved.
	Direction ParentEdgeDirection `yaml:"direction,omitempty"`
}

// EffectiveDirection returns the declared direction with the outbound default
// applied, matching the Effective* convention used elsewhere in this file.
func (p ParentEdgeSpec) EffectiveDirection() ParentEdgeDirection {
	return NormalizeDirection(p.Direction)
}

// UnmarshalYAML accepts either a bare scalar (the edge name, direction defaulting
// to outbound) or a mapping with explicit edge / direction keys.
//
// The mapping branch walks the node's keys by hand rather than calling
// value.Decode into a shadow struct, and that is load-bearing rather than
// stylistic: yaml.v3's Node.Decode does NOT inherit the parent decoder's
// KnownFields setting, so an unknown key here would be silently dropped even
// under the strict decoder the platform installer uses. A mistyped "directon:
// in" would then leave Direction empty and default to OUTBOUND — reintroducing
// the exact silent wrong-direction failure this field exists to prevent, and
// reintroducing it in the one place an author was trying to be explicit.
func (p *ParentEdgeSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var edge string
		if err := value.Decode(&edge); err != nil {
			return fmt.Errorf("parent_edges entry: %w", err)
		}
		p.Edge = edge
		p.Direction = ""
		return nil

	case yaml.MappingNode:
		for i := 0; i+1 < len(value.Content); i += 2 {
			key, val := value.Content[i], value.Content[i+1]
			switch key.Value {
			case "edge":
				if err := val.Decode(&p.Edge); err != nil {
					return fmt.Errorf("parent_edges entry: edge: %w", err)
				}
			case "direction":
				var dir string
				if err := val.Decode(&dir); err != nil {
					return fmt.Errorf("parent_edges entry: direction: %w", err)
				}
				p.Direction = ParentEdgeDirection(dir)
			default:
				return fmt.Errorf(
					"parent_edges entry: unknown field %q (expected edge, direction); "+
						"a mistyped direction key would silently default to %q",
					key.Value, ParentEdgeOut,
				)
			}
		}
		return nil

	default:
		return fmt.Errorf(
			"parent_edges entry must be an edge name or a mapping with edge/direction, got YAML kind %d",
			value.Kind,
		)
	}
}

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
	// For a Hierarchical type it applies WITHIN a containment level only —
	// batches never span levels. That is safe because a level-n row's parent is
	// at level n-1 by construction, so intra-level concurrency cannot violate
	// ancestor-before-descendant ordering. Zero or less ⇒ 1 (sequential). Use
	// EffectiveMaxInFlight to resolve.
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
	// Name is the entity type to push, as the SOURCE-NATIVE CLASS ID — the
	// identifier the source system uses, verbatim, and the exact key under which
	// the type is registered in the tenant's ontology catalog.
	//
	// It may contain a colon (`brick:AHU`, `rec:Zone`) or be colon-free
	// (`Equipment`, `Location`). Both are class IDs; a colon-free name is not a
	// simplified spelling of a namespaced one, so the two forms name DIFFERENT
	// catalog entries. Changing `Equipment` to `brick:Equipment` therefore changes
	// WHICH entities get pushed — it is not a notational tidy-up.
	//
	// The comparison at install time is exact. Do not write the platform's internal
	// storage identifier here: what gets pushed is chosen by class ID, and where
	// those rows physically live is a platform concern.
	//
	// The type must exist in the tenant's active ontology at install time — a check
	// the platform performs and the SDK deliberately does not, since it needs tenant
	// state no local lint can see.
	Name string `yaml:"name"`

	// ParentEdges names the edges that identify a pushed record's OWNER — the
	// single entity it is subordinate to in the external system.
	//
	// Each entry is an edge plus a TRAVERSAL DIRECTION, and YAML accepts a bare
	// edge name as shorthand for the outbound default (see ParentEdgeSpec):
	//
	//	parent_edges: [hasLocation]        # child holds the edge ⇒ out(hasLocation)
	//	parent_edges:
	//	  - edge: hasPoint                 # parent holds the edge ⇒ in(hasPoint)
	//	    direction: in
	//
	// Get the direction RIGHT, because getting it wrong is silent rather than
	// loud. Traversing the wrong way resolves to nothing, and "no owner" is a
	// legal, common answer meaning "this record is a root" — so every affected
	// record is pushed with no foreign key and the run reports complete success.
	// Declaring the INVERSE predicate instead is the louder mistake: it presents
	// as fan-out, one level resolving to hundreds of targets, and the platform
	// fails the batch rather than choosing one.
	//
	// Which direction a kit needs is a fact about how its loader STORES the edge,
	// not about how the source system words it. A loader that normalizes inverse
	// predicates to one canonical edge decides the stored orientation, so read the
	// loader rather than the source vocabulary.
	//
	// The platform resolves each declared edge at read time and sends the result
	// as parent_refs on every pushed entity (see the sibling egress package).
	// That is the ONLY way a component can populate an external foreign key: the
	// entity as read carries no containment, because containment is an edge and
	// an entity read projects columns.
	//
	// Declare it on every type whose external record references another. It is
	// independent of Hierarchical: an edge to a DIFFERENT type (equipment to its
	// containing location) needs a reference but no level walk.
	//
	// Each name MUST already be a legal ArcadeDB local identifier — only
	// [a-zA-Z0-9_], and no trailing underscore. Unlike Name above, these are NOT
	// catalog keys compared as opaque strings: the platform composes them into
	// type identifiers, and it materializes a predicate under a SANITIZED name
	// (every other character becomes "_", trailing "_" trimmed). So a declared
	// "rec:hasPart" addresses "{tenant}_rec:hasPart" — a type with no edges — and
	// the walk finds no children, pushing ONLY THE ROOTS while reporting a
	// complete run. Declare the sanitized form ("rec_hasPart").
	//
	// That asymmetry is deliberate and easy to get backwards: a colon is ordinary
	// in Name and fatal here.
	ParentEdges []ParentEdgeSpec `yaml:"parent_edges,omitempty"`

	// Hierarchical declares that this type's owner edge is SELF-REFERENCING, so
	// the platform walks it level by level, roots first, instead of paging by id.
	//
	// It must be declared and CANNOT be inferred, which is the part worth
	// understanding rather than accepting. The same predicate plays both roles: a
	// campus kit's hasLocation is self-referencing for a location type (room to
	// level) and cross-type for an equipment type (equipment to location). So
	// hierarchical-ness is a property of the (declaring type, edge) PAIR, and no
	// attribute of the edge itself could carry it. Nor can the ontology catalog
	// answer it — such a predicate is typically declared with wildcard endpoints.
	//
	// Inferring it from stored data was considered and rejected: probing whether
	// out(edge) lands in the same physical type is correct on a fully-loaded
	// tenant and silently WRONG on a partially-loaded one, where no row has a
	// parent yet, so the probe concludes "not hierarchical" and children are
	// pushed before parents. Control flow must not be inferred from data that can
	// legitimately be incomplete.
	//
	// The walk follows the edge's declared DIRECTION, so a self-referencing edge
	// stored parent to child is walked inbound. Requires exactly one ParentEdges
	// entry: "level" is hops along a SINGLE containment axis, so with two declared
	// edges a row's depth is not well defined and there is no non-arbitrary way to
	// pick the axis. Use HierarchyEdge to read the resolved pair.
	Hierarchical bool `yaml:"hierarchical,omitempty"`

	// IncludeDescendants selects every entity type stored under the same physical
	// type as Name, rather than only Name itself.
	//
	// It exists because a kit may store many fine-grained classes under a few
	// coarse physical types, so "the declared entity type" is otherwise
	// ambiguous. Declare the coarse type with this flag and a class added to the
	// source later is picked up with NO manifest change — the property a
	// continuously-authored twin needs.
	//
	// The platform composes one batch per (level, entity type) so each batch
	// stays homogeneous, and it applies the same selection rule to change
	// delivery as to the bulk push. Absent ⇒ Name selects exactly itself.
	IncludeDescendants bool `yaml:"include_descendants,omitempty"`
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

// EffectiveMaxInFlight returns the declared batch concurrency, defaulted.
//
// It takes no entity type and applies no clamp. An earlier shape forced 1 for a
// hierarchical type, on the reasoning that concurrent batches could push a child
// before its parent. That is true only ACROSS levels: within one level every
// row's parent sits at the level above, already pushed, so intra-level
// concurrency is safe. Confining batches to a level is the platform's scheduling
// job and cannot be expressed as a single number, so this helper reports what the
// kit asked for and does not pretend to encode the constraint.
func (e *EgressSyncSpec) EffectiveMaxInFlight() int {
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

// ParentEdgeNames returns the declared owner-edge names in declared order,
// skipping empty entries.
//
// Direction-blind by construction, so use it only where the NAMES are what
// matter — reporting, or matching against a component's known edge set. Anything
// that resolves an owner must read EffectiveDirection too, or it will traverse a
// parent-to-child edge the wrong way and silently find nothing.
func (t *EgressEntityTypeSpec) ParentEdgeNames() []string {
	if len(t.ParentEdges) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.ParentEdges))
	for i := range t.ParentEdges {
		if e := t.ParentEdges[i].Edge; e != "" {
			out = append(out, e)
		}
	}
	return out
}

// HierarchyEdge returns the edge the level-by-level walk traverses, with its
// direction resolved, and whether this type is walked hierarchically at all.
//
// It is the single resolution point for "which edge orders the walk, and which
// way", so a kit's own tooling and the platform cannot disagree about it.
// Validation guarantees a hierarchical type declares exactly one edge, so the
// answer is unambiguous by construction rather than by picking a winner.
func (t *EgressEntityTypeSpec) HierarchyEdge() (ParentEdgeSpec, bool) {
	if t == nil || !t.Hierarchical || len(t.ParentEdges) != 1 {
		return ParentEdgeSpec{}, false
	}
	edge := t.ParentEdges[0]
	edge.Direction = edge.EffectiveDirection()
	return edge, true
}

// validateEgressSyncs checks each kit-declared egress component
// (spec.egress_syncs[]): unique name, external_system present, at least one
// entity type, a digest-pinned image, and coherent per-type edge declarations. It mirrors
// the platform's domainkit.validateEgressSpecStructure — the checks that need no
// external state — so a kit author gets the same rejection locally that the
// installer would raise as OGA-EGRS-VAL-1002.
//
// The per-entity-type checks (validateEgressEntityType) are the one addition the
// platform does NOT make at install: it validates an edge name only when the
// Day-1 walk reaches it (egress.assertAddressableEdge), so an unaddressable edge
// installs cleanly and fails mid-run, having pushed only the roots. None of it
// needs tenant state, so catching it here is strictly better than catching it
// there.
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
		for j := range e.EntityTypes {
			if err := validateEgressEntityType(i, j, &e.EntityTypes[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateEgressEntityType checks one entity_types[] entry: that Hierarchical is
// coherent with ParentEdges, that every declared direction is one the platform can
// traverse and no edge is declared in both, and that every declared edge name can
// actually address an edge type.
//
// None of this needs tenant state, which is why it belongs here. The platform
// validates edge addressability only when the Day-1 walk reaches it, so an
// unaddressable edge otherwise installs cleanly and fails mid-run having pushed
// only the roots.
func validateEgressEntityType(i, j int, et *EgressEntityTypeSpec) error {
	// A level walk has to traverse something. Hierarchical with no edge is not a
	// conservative default, it is incoherent: the platform would look for roots
	// via an edge that was never named.
	if et.Hierarchical && len(et.ParentEdges) == 0 {
		return fmt.Errorf(
			"spec.egress_syncs[%d].entity_types[%d]: hierarchical is set but parent_edges is empty — "+
				"a level-by-level walk has no edge to traverse",
			i, j,
		)
	}
	// A hierarchical type's "level" is hops along a SINGLE containment axis, so
	// several declared edges leave a row's depth undefined. Silently taking the
	// first was rejected: it yields a plausible-looking run ordered along whichever
	// edge happens to be first, which is the silent wrongness the explicit
	// hierarchical flag exists to prevent. A NON-hierarchical type may declare as
	// many owner edges as it needs — it wants each owner referenced but no level
	// walk, so there is no axis to disambiguate.
	if et.Hierarchical && len(et.ParentEdges) > 1 {
		return fmt.Errorf(
			"spec.egress_syncs[%d].entity_types[%d]: hierarchical is set with %d parent_edges; "+
				"exactly one is required because level is defined by hops along a single containment axis",
			i, j, len(et.ParentEdges),
		)
	}
	// Keyed by edge NAME, not by (name, direction): both directions of one edge
	// resolve into the same parent_refs key, so they collide even though they are
	// not literally the same declaration. The two cases get distinct messages
	// because their remedies differ — drop a redundant line, versus decide which
	// way the edge actually points.
	seen := make(map[string]ParentEdgeDirection, len(et.ParentEdges))
	for _, pe := range et.ParentEdges {
		edge := pe.Edge
		if strings.TrimSpace(edge) == "" {
			return fmt.Errorf(
				"spec.egress_syncs[%d].entity_types[%d]: parent_edges contains an empty entry",
				i, j,
			)
		}
		if !pe.Direction.Valid() {
			return fmt.Errorf(
				"spec.egress_syncs[%d].entity_types[%d]: parent_edges entry %q declares direction %q; "+
					"expected %q (the declaring entity holds the edge) or %q (the owner holds it)",
				i, j, edge, pe.Direction, ParentEdgeOut, ParentEdgeIn,
			)
		}
		dir := pe.EffectiveDirection()
		if prev, dup := seen[edge]; dup {
			if prev == dir {
				return fmt.Errorf(
					"spec.egress_syncs[%d].entity_types[%d]: parent_edges lists %q twice; each edge is one "+
						"parent_refs key",
					i, j, edge,
				)
			}
			return fmt.Errorf(
				"spec.egress_syncs[%d].entity_types[%d]: parent_edges declares %q in both directions "+
					"(%q and %q); both resolve into the one parent_refs key %q, so which owner wins is "+
					"undefined — declare the direction the edge is actually stored in",
				i, j, edge, prev, dir, edge,
			)
		}
		seen[edge] = dir

		if isAddressableEdgeName(edge) {
			continue
		}
		if suggestion := sanitizeEdgeName(edge); suggestion != "" {
			return fmt.Errorf(
				"spec.egress_syncs[%d].entity_types[%d]: parent_edges entry %q is not addressable by "+
					"its declared name — the platform materializes a predicate under a sanitized "+
					"identifier ([a-zA-Z0-9_] only, trailing underscores trimmed), so a walk on this "+
					"name would find no children and would push only the roots; declare %q",
				i, j, edge, suggestion,
			)
		}
		return fmt.Errorf(
			"spec.egress_syncs[%d].entity_types[%d]: parent_edges entry %q has no legal identifier form "+
				"(it sanitizes to the empty string), so it can never name an edge type",
			i, j, edge,
		)
	}
	return nil
}

// isAddressableEdgeName reports whether name is ALREADY what the platform's
// identifier sanitizer would produce for it — only [a-zA-Z0-9_], and no trailing
// underscore, which that sanitizer trims.
//
// Expressed as a validity predicate rather than as "sanitize and compare" on
// purpose. The platform learned this the hard way: its first version of the same
// guard enumerated the characters it thought were unsafe (":" and "-") and
// silently let "has.location" through. Stating what is LEGAL cannot drift that
// way, because any name the sanitizer would alter is by definition not already
// legal.
func isAddressableEdgeName(name string) bool {
	if name == "" || strings.HasSuffix(name, "_") {
		return false
	}
	for _, ch := range name {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_':
		default:
			return false
		}
	}
	return true
}

// sanitizeEdgeName mirrors the platform's identifier sanitizer so a rejection can
// name the exact form the author should declare instead. It returns "" when the
// name has no legal form at all (every character replaced, then trimmed away).
func sanitizeEdgeName(name string) string {
	var sb strings.Builder
	sb.Grow(len(name))
	for _, ch := range name {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_':
			sb.WriteRune(ch)
		default:
			sb.WriteRune('_')
		}
	}
	return strings.TrimRight(sb.String(), "_")
}
