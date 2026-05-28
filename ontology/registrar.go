package ontology

import "context"

// OntologyRegistrar provides type registration that hides the complexity of
// versioning, activation, and embedding generation. Kit developers call
// RegisterTypes once during installation; the platform handles the rest.
type OntologyRegistrar interface {
	// RegisterTypes registers entity and relationship types in the platform's
	// ontology layer. The platform will:
	//   1. Create or update the ontology version
	//   2. Activate the version (generating embeddings for semantic search)
	//   3. Publish events to refresh the schema cache
	//
	// This operation is idempotent — re-registering the same types is safe.
	RegisterTypes(ctx context.Context, req RegisterTypesRequest) error

	// GetRegisteredTypes returns all types previously registered by a kit.
	GetRegisteredTypes(ctx context.Context, kitID string) ([]RegisteredType, error)
}

// RegisterTypesRequest contains the types to register in the ontology.
type RegisterTypesRequest struct {
	// KitID identifies the kit that owns these types.
	KitID string

	// TenantID is the tenant for which types are being registered.
	TenantID string

	// EntityTypes are the entity type definitions to register.
	EntityTypes []EntityTypeDef

	// TypeHierarchy defines parent-child relationships between types.
	TypeHierarchy []TypeHierarchyEntry
}

// EntityTypeDef defines an entity type for the semantic ontology layer.
// This is the logical definition used for type discovery and semantic search,
// distinct from the physical DDL schema (which is handled by SchemaManager).
type EntityTypeDef struct {
	// Name is the stable identifier for this type (e.g., "brick_Equipment").
	// Must match the DDL type name (without tenant prefix).
	Name string

	// DisplayName is the human-readable name, keyed by locale.
	// Example: {"en": "Equipment (Brick)", "vi": "Thiết Bị (Brick)"}
	DisplayName map[string]string

	// Description is a detailed description, keyed by locale.
	// The en-US description is used for embedding generation.
	Description map[string]string

	// ParentType is the name of the parent type in the hierarchy.
	// Empty string means this is a root type.
	ParentType string

	// Properties lists the domain-specific properties of this type.
	// Used to build the embedding text: "{Name}: {Description} (properties: ...)"
	Properties []TypeProperty

	// Category classifies this type (e.g., "equipment", "location", "custom").
	Category string
}

// TypeProperty describes a property for ontology registration purposes.
type TypeProperty struct {
	// Name is the property identifier (snake_case).
	Name string

	// Description is a human-readable description, keyed by locale.
	Description map[string]string

	// Type is the data type (string, integer, float, boolean, datetime).
	Type string

	// Required indicates whether this property is mandatory.
	Required bool
}

// TypeHierarchyEntry defines a parent-child relationship between types.
type TypeHierarchyEntry struct {
	// TypeName is the child type.
	TypeName string

	// ParentType is the parent type.
	ParentType string
}

// RegisteredType represents a type that has been registered in the ontology.
type RegisteredType struct {
	// Name is the type identifier.
	Name string

	// KitID is the kit that registered this type.
	KitID string

	// IsActive indicates whether this type is currently active.
	IsActive bool

	// HasEmbedding indicates whether an embedding has been generated.
	HasEmbedding bool
}
