package transfer

// OntologySnapshot is the full desired-ontology wire contract a Source
// Connector submits to the platform's gated ontology-refresh intake (OGA-634).
// It is the FIRST-CLASS, versioned kit-author type for that submission: a kit
// connector builds one and json.Marshals it directly, so the platform decode
// can never silently drop a field the way a hand-mirrored copy of the
// platform's internal structs could (OGA-636).
//
// It reuses the same element types the transfer writer already ships
// (EntityTypeDef / RelationshipTypeDef / HierarchyEntry), so the rich,
// drift-prone fields (localized DisplayName/Description, Materialization,
// PhysicalType, cardinality, properties) live in ONE shared, snake_case-tagged
// place. The platform maps this envelope onto its internal ontology types
// server-side at the intake edge.
//
// Unlike the streaming ontology artifact (Header + NDJSON Envelope records,
// consumed incrementally by a kind=ontology loader), a snapshot is a single
// self-contained JSON document diffed as a whole A→B transition against the
// tenant's active ontology.
type OntologySnapshot struct {
	// EntityTypes is the complete set of entity types the tenant's ontology
	// should converge to within the governed scope. Required (an empty snapshot
	// is a no-op the platform treats as "unchanged").
	EntityTypes []EntityTypeDef `json:"entity_types"`

	// RelationshipTypes is the complete set of relationship (edge) types in
	// scope. A pure Brick/REC class feed produces none (predicates are the
	// kit's declarative ontology YAML), so it is typically empty.
	RelationshipTypes []RelationshipTypeDef `json:"relationship_types,omitempty"`

	// Hierarchy is the explicit parent→child edge set, letting the platform
	// validate the type graph (no cycles, no missing parents) before applying.
	// It is also encoded on each EntityTypeDef.ParentType.
	Hierarchy []HierarchyEntry `json:"hierarchy,omitempty"`

	// GovernedPrefixes scopes what the snapshot is AUTHORITATIVE over for
	// removal detection. A connector typically governs only a subset of the
	// tenant's ontology (e.g. the logical Brick/REC leaf catalog, type names
	// prefixed "brick_"/"rec_") — NOT the kit's declarative physical types
	// (Equipment/Location/…). When set, a removal is only considered for an
	// active type whose name has one of these prefixes; the snapshot's own
	// types are always in scope. Empty ⇒ the snapshot governs the whole
	// ontology (a full A→B diff).
	GovernedPrefixes []string `json:"governed_prefixes,omitempty"`
}
