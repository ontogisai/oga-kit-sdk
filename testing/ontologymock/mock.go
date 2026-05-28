// Package ontologymock provides a mock OntologyRegistrar for testing kit code
// without a running platform.
package ontologymock

import (
	"context"
	"sync"

	"github.com/ontogisai/oga-kit-sdk/ontology"
)

// MockOntologyRegistrar is an in-memory mock of ontology.OntologyRegistrar.
type MockOntologyRegistrar struct {
	mu              sync.Mutex
	registeredTypes map[string][]ontology.EntityTypeDef // keyed by kitID
	err             error
}

// NewOntologyRegistrar creates a new mock ontology registrar.
func NewOntologyRegistrar() *MockOntologyRegistrar {
	return &MockOntologyRegistrar{
		registeredTypes: make(map[string][]ontology.EntityTypeDef),
	}
}

// SetError configures an error to return on all operations.
func (m *MockOntologyRegistrar) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// RegisterTypes records the type registration.
func (m *MockOntologyRegistrar) RegisterTypes(_ context.Context, req ontology.RegisterTypesRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.registeredTypes[req.KitID] = append(m.registeredTypes[req.KitID], req.EntityTypes...)
	return nil
}

// GetRegisteredTypes returns types registered by a kit.
func (m *MockOntologyRegistrar) GetRegisteredTypes(_ context.Context, kitID string) ([]ontology.RegisteredType, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}

	defs := m.registeredTypes[kitID]
	result := make([]ontology.RegisteredType, 0, len(defs))
	for _, d := range defs {
		result = append(result, ontology.RegisteredType{
			Name:         d.Name,
			KitID:        kitID,
			IsActive:     true,
			HasEmbedding: true,
		})
	}
	return result, nil
}

// GetAllRegistered returns all registered types across all kits.
func (m *MockOntologyRegistrar) GetAllRegistered() map[string][]ontology.EntityTypeDef {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string][]ontology.EntityTypeDef, len(m.registeredTypes))
	for k, v := range m.registeredTypes {
		cp := make([]ontology.EntityTypeDef, len(v))
		copy(cp, v)
		result[k] = cp
	}
	return result
}

// RegisterCount returns the total number of types registered.
func (m *MockOntologyRegistrar) RegisterCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, types := range m.registeredTypes {
		count += len(types)
	}
	return count
}
