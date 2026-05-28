// Package schema defines the SchemaManager interface and types for DDL schema
// management. Kit developers use these types to declare vertex and edge types
// that the platform creates in the knowledge graph.
//
// All vertex types created via SchemaManager automatically EXTEND BaseEntity
// (inheriting tenant_id, audit fields, bi-temporal fields, H3 spatial columns).
// All edge types automatically EXTEND BaseRelationship. Kit developers only
// declare domain-specific properties.
package schema
