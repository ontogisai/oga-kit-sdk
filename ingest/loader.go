package ingest

import "context"

// DataLoader transforms proprietary data formats into platform-standard
// vertices and edges. Kit developers implement this interface to support
// custom import formats.
//
// The platform calls Load with raw data bytes; the loader returns structured
// vertices and edges that the platform persists, indexes, and embeds.
type DataLoader interface {
	// Load parses the input data and returns vertices and edges.
	// The tenantID is provided for any tenant-specific logic.
	Load(ctx context.Context, tenantID string, data []byte) (*LoadResult, error)

	// Format returns the format identifier this loader handles
	// (e.g., "brick-campus-json", "ifc-step", "sap-pm-export").
	Format() string
}

// LoadResult contains the parsed vertices and edges from a data load operation.
type LoadResult struct {
	// Vertices are the entity instances to create or update.
	Vertices []Vertex

	// Edges are the relationships to create or update.
	Edges []Edge

	// Warnings are non-fatal issues encountered during parsing.
	Warnings []string
}

// Vertex represents an entity instance to be persisted in the knowledge graph.
type Vertex struct {
	// ID is a stable, deterministic identifier for this entity.
	// If empty, the platform generates one from the properties.
	ID string

	// EntityType is the type name (must match a registered type, e.g., "brick_Equipment").
	EntityType string

	// Label is a human-readable label for this entity instance.
	Label string

	// Properties are the domain-specific property values.
	Properties map[string]any

	// Latitude is the WGS84 latitude (optional, for spatial indexing).
	Latitude *float64

	// Longitude is the WGS84 longitude (optional, for spatial indexing).
	Longitude *float64
}

// Edge represents a relationship instance between two entities.
type Edge struct {
	// ID is a stable, deterministic identifier for this relationship.
	// If empty, the platform generates one from source + target + type.
	ID string

	// RelationshipType is the edge type name (e.g., "hasLocation", "feeds").
	RelationshipType string

	// SourceID is the ID of the source vertex.
	SourceID string

	// TargetID is the ID of the target vertex.
	TargetID string

	// Properties are optional edge properties.
	Properties map[string]any
}
