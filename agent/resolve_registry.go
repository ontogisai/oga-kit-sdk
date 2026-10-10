package agent

// DefaultRegistry maps each first-party MCP tool that produces identifiers to
// its output shape. It is the single source of truth for dependent-step
// resolution, citation extraction, and result truncation.
//
// Adding a new tool, or adapting to a changed output shape, is a one-line entry
// here plus a test — no extractor logic changes. Shapes are captured from the
// handler output structs in oga-platform/internal/mcptoolserver.
var DefaultRegistry = Registry{
	// kg_search → {"results":[{"entity_id":...}]}  (handler_search.go: SearchResult)
	"kg_search": {
		ArrayKeys:    []string{"results"},
		ItemIDFields: []string{"entity_id", "id"},
		Kind:         IDKindEntity,
	},
	// kg_vector → {"results":[{"entity_id":...}]} (similar/nearest) or
	// {"clusters":[{"representative_entity":...}]} (cluster). (handler_vector.go)
	"kg_vector": {
		ArrayKeys:    []string{"results", "clusters"},
		ItemIDFields: []string{"entity_id", "representative_entity"},
		Kind:         IDKindEntity,
	},
	// kg_query_entities → {"entities":[{"id":...}]} (handler_entity.go: entityResult)
	"kg_query_entities": {
		ArrayKeys:    []string{"entities"},
		ItemIDFields: []string{"id", "entity_id"},
		Kind:         IDKindEntity,
	},
	// kg_get_entity → {"entity_id":...} / {"id":...} (single object)
	"kg_get_entity": {
		SingleIDFields: []string{"entity_id", "id"},
		Kind:           IDKindEntity,
	},
	// kg_reason → {"results":[{"matched_entities":[...],"path":[{"entity_id":...}]}]}
	// (handler_reason.go: ReasonResult). No scalar item ID — IDs are nested.
	"kg_reason": {
		ArrayKeys:   []string{"results"},
		NestedPaths: [][]string{{"path", "entity_id"}, {"matched_entities"}},
		Kind:        IDKindEntity,
	},
	// kg_traverse → {"nodes":[{"id":...}]} (handler_traverse.go: traversalNode).
	// The result also echoes the input seed as "start_entity_id"; that is the
	// caller's own input, not a new identifier, so it is intentionally NOT
	// extracted — dependent steps want a discovered node.
	"kg_traverse": {
		ArrayKeys:    []string{"nodes"},
		ItemIDFields: []string{"id", "entity_id"},
		Kind:         IDKindEntity,
	},
	// kg_query_relationships → {"relationships":[{"source_entity_id":...,
	// "target_entity_id":...}]} (handler_relationship.go: relationshipResult).
	// Resolves to the endpoint entities, kind entity.
	"kg_query_relationships": {
		ArrayKeys:    []string{"relationships"},
		ItemIDFields: []string{"source_entity_id", "target_entity_id"},
		Kind:         IDKindEntity,
	},
	// kg_doc_search → {"documents":[{"id":...}]} (handler_document.go: DocumentResult)
	"kg_doc_search": {
		ArrayKeys:    []string{"documents"},
		ItemIDFields: []string{"id"},
		Kind:         IDKindDocument,
	},
	// kg_doc_content → {"passages":[{"document_id":...}]} (handler_document.go: ContentPassage)
	"kg_doc_content": {
		ArrayKeys:    []string{"passages"},
		ItemIDFields: []string{"document_id"},
		Kind:         IDKindDocument,
	},
}

// lookupDescriptor returns the registry descriptor for tool, or nil when the
// tool is unknown (the caller then falls back to the heuristic scan).
func lookupDescriptor(tool string) *ShapeDescriptor {
	if tool == "" {
		return nil
	}
	if d, ok := DefaultRegistry[tool]; ok {
		return &d
	}
	return nil
}

// ArrayKeysForTool returns the candidate result array keys a tool emits, for
// consumers that need to operate on the tool's own output array (e.g.
// MaxResults truncation). Returns nil for unknown tools.
func ArrayKeysForTool(tool string) []string {
	if d, ok := DefaultRegistry[tool]; ok {
		return d.ArrayKeys
	}
	return nil
}
