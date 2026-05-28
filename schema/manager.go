package schema

import "context"

// SchemaManager provides safe DDL abstraction for kit developers.
// All vertex types created via this interface automatically EXTEND BaseEntity.
// All edge types automatically EXTEND BaseRelationship.
// Kit developers never need to declare tenant_id, audit fields, bi-temporal
// fields, or H3 spatial columns — they are inherited from the base types.
type SchemaManager interface {
	// CreateVertexType creates a new vertex type in the knowledge graph.
	// The type automatically inherits all BaseEntity fields.
	CreateVertexType(ctx context.Context, def VertexTypeDef) error

	// CreateEdgeType creates a new edge type in the knowledge graph.
	// The type automatically inherits all BaseRelationship fields.
	CreateEdgeType(ctx context.Context, def EdgeTypeDef) error

	// AddProperty adds a new property to an existing type.
	AddProperty(ctx context.Context, typeName string, prop PropertyDef) error

	// AddIndex adds an index to an existing type.
	AddIndex(ctx context.Context, typeName string, index IndexDef) error

	// TypeExists checks whether a type already exists in the schema.
	TypeExists(ctx context.Context, typeName string) (bool, error)
}

// VertexTypeDef defines a vertex type to be created in the knowledge graph.
type VertexTypeDef struct {
	// Name is the type name (e.g., "brick_Equipment", "WorkOrder").
	// Will be prefixed with the tenant identifier at creation time.
	Name string

	// Properties are the domain-specific properties for this type.
	// Infrastructure properties (id, tenant_id, audit, bi-temporal, H3) are
	// inherited from BaseEntity and should NOT be declared here.
	Properties []PropertyDef

	// Indexes are additional indexes beyond the default ones inherited
	// from BaseEntity.
	Indexes []IndexDef

	// Spatial configures spatial indexing for this type.
	// If nil, no additional spatial configuration is applied beyond the
	// default H3 columns inherited from BaseEntity.
	Spatial *SpatialConfig
}

// EdgeTypeDef defines an edge type to be created in the knowledge graph.
type EdgeTypeDef struct {
	// Name is the edge type name (e.g., "hasLocation", "feeds").
	// Will be prefixed with the tenant identifier at creation time.
	Name string

	// Properties are the domain-specific properties for this edge type.
	// Infrastructure properties (tenant_id, audit, bi-temporal) are
	// inherited from BaseRelationship and should NOT be declared here.
	Properties []PropertyDef

	// Indexes are additional indexes for this edge type.
	Indexes []IndexDef
}

// PropertyDef defines a single property on a vertex or edge type.
type PropertyDef struct {
	// Name is the property name (snake_case).
	Name string

	// Type is the property data type.
	Type PropertyType

	// Required indicates whether this property must be set on every instance.
	Required bool

	// Description is a human-readable description of the property.
	Description string
}

// PropertyType enumerates the supported property data types.
type PropertyType string

const (
	PropertyTypeString      PropertyType = "STRING"
	PropertyTypeInteger     PropertyType = "INTEGER"
	PropertyTypeLong        PropertyType = "LONG"
	PropertyTypeFloat       PropertyType = "FLOAT"
	PropertyTypeDouble      PropertyType = "DOUBLE"
	PropertyTypeBoolean     PropertyType = "BOOLEAN"
	PropertyTypeDatetime    PropertyType = "DATETIME"
	PropertyTypeEmbedded    PropertyType = "EMBEDDED"
	PropertyTypeList        PropertyType = "LIST"
	PropertyTypeListOfFloat PropertyType = "LIST OF FLOAT"
)

// IndexDef defines an index on a type.
type IndexDef struct {
	// Properties are the property names included in this index.
	Properties []string

	// Type is the index type.
	Type IndexType
}

// IndexType enumerates the supported index types.
type IndexType string

const (
	IndexTypeUnique    IndexType = "UNIQUE"
	IndexTypeNotUnique IndexType = "NOTUNIQUE"
	IndexTypeFullText  IndexType = "FULL_TEXT"
	IndexTypeVector    IndexType = "LSM_VECTOR"
)

// SpatialConfig configures spatial indexing for a vertex type.
type SpatialConfig struct {
	// Enabled indicates whether spatial indexing is active for this type.
	Enabled bool

	// Resolutions specifies which H3 resolutions to index.
	// If empty, the platform default resolutions are used (4, 6, 8, 10, 12, 15).
	Resolutions []int
}
