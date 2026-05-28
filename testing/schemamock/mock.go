// Package schemamock provides a mock SchemaManager for testing kit code
// without a running platform.
package schemamock

import (
	"context"
	"sync"

	"github.com/ontogisai/oga-kit-sdk/schema"
)

// MockSchemaManager is an in-memory mock of schema.SchemaManager.
type MockSchemaManager struct {
	mu          sync.Mutex
	vertexTypes map[string]schema.VertexTypeDef
	edgeTypes   map[string]schema.EdgeTypeDef
	properties  map[string][]schema.PropertyDef
	indexes     map[string][]schema.IndexDef
	errors      map[string]error
}

// NewSchemaManager creates a new mock schema manager.
func NewSchemaManager() *MockSchemaManager {
	return &MockSchemaManager{
		vertexTypes: make(map[string]schema.VertexTypeDef),
		edgeTypes:   make(map[string]schema.EdgeTypeDef),
		properties:  make(map[string][]schema.PropertyDef),
		indexes:     make(map[string][]schema.IndexDef),
		errors:      make(map[string]error),
	}
}

// SetError configures an error to return for a specific type name.
func (m *MockSchemaManager) SetError(typeName string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors[typeName] = err
}

// CreateVertexType records the vertex type creation.
func (m *MockSchemaManager) CreateVertexType(_ context.Context, def schema.VertexTypeDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errors[def.Name]; ok {
		return err
	}
	m.vertexTypes[def.Name] = def
	return nil
}

// CreateEdgeType records the edge type creation.
func (m *MockSchemaManager) CreateEdgeType(_ context.Context, def schema.EdgeTypeDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errors[def.Name]; ok {
		return err
	}
	m.edgeTypes[def.Name] = def
	return nil
}

// AddProperty records a property addition.
func (m *MockSchemaManager) AddProperty(_ context.Context, typeName string, prop schema.PropertyDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errors[typeName]; ok {
		return err
	}
	m.properties[typeName] = append(m.properties[typeName], prop)
	return nil
}

// AddIndex records an index addition.
func (m *MockSchemaManager) AddIndex(_ context.Context, typeName string, index schema.IndexDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errors[typeName]; ok {
		return err
	}
	m.indexes[typeName] = append(m.indexes[typeName], index)
	return nil
}

// TypeExists checks if a type has been created.
func (m *MockSchemaManager) TypeExists(_ context.Context, typeName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errors[typeName]; ok {
		return false, err
	}
	_, vOK := m.vertexTypes[typeName]
	_, eOK := m.edgeTypes[typeName]
	return vOK || eOK, nil
}

// GetVertexTypes returns all created vertex types.
func (m *MockSchemaManager) GetVertexTypes() map[string]schema.VertexTypeDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]schema.VertexTypeDef, len(m.vertexTypes))
	for k, v := range m.vertexTypes {
		result[k] = v
	}
	return result
}

// GetEdgeTypes returns all created edge types.
func (m *MockSchemaManager) GetEdgeTypes() map[string]schema.EdgeTypeDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]schema.EdgeTypeDef, len(m.edgeTypes))
	for k, v := range m.edgeTypes {
		result[k] = v
	}
	return result
}
