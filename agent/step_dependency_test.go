package agent

import "testing"

// TestResolveDependentArgs_SearchResultsShape covers the kg_search shape:
// {"results":[{"entity_id":...}]}.
func TestResolveDependentArgs_SearchResultsShape(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"entity_id":"019e38e3-69b8-7b61-849b-5a78e816e244","entity_type":"brick_Chiller","label":"CH-36A"}],"total_count":1}`
	args := map[string]any{"entity_id": "<from step 0>"}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["entity_id"] != "019e38e3-69b8-7b61-849b-5a78e816e244" {
		t.Errorf("entity_id = %q, want resolved UUID", resolved["entity_id"])
	}
}

// TestResolveDependentArgs_QueryEntitiesShape reproduces the kg_traverse bug:
// the prior step (kg_query_entities) returns {"entities":[{"id":...}]}, a shape
// the resolver previously ignored, so the "<from step 1>" placeholder reached
// kg_traverse verbatim and failed with OGA-MCPG-NFND-1301.
func TestResolveDependentArgs_QueryEntitiesShape(t *testing.T) {
	t.Parallel()

	prior := `{"entities":[{"id":"019e38e3-69b8-7b61-849b-5a78e816e244","entity_type":"brick_Chiller","tenant_id":"sgac1","properties":{}}],"count":1}`
	args := map[string]any{
		"start_entity_id":   "<from step 1>",
		"relationship_type": "hasPoint",
		"direction":         "outgoing",
		"max_depth":         1,
	}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["start_entity_id"] != "019e38e3-69b8-7b61-849b-5a78e816e244" {
		t.Errorf("start_entity_id = %q, want resolved UUID from entities[].id", resolved["start_entity_id"])
	}
}

// TestResolveDependentArgs_EntitiesShape_AutoResolveEmpty verifies the entities
// array also feeds auto-resolution when the ID field is absent entirely.
func TestResolveDependentArgs_EntitiesShape_AutoResolveEmpty(t *testing.T) {
	t.Parallel()

	prior := `{"entities":[{"id":"ent-aaa-001","entity_type":"brick_AHU"}],"count":1}`
	args := map[string]any{}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["start_entity_id"] != "ent-aaa-001" {
		t.Errorf("start_entity_id = %q, want auto-resolved from entities[].id", resolved["start_entity_id"])
	}
}

// TestResolveDependentArgs_ResultsArray_IDFallback verifies the results array
// also reads "id" when "entity_id" is absent on the item.
func TestResolveDependentArgs_ResultsArray_IDFallback(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"id":"res-id-001","entity_type":"brick_VAV"}]}`
	args := map[string]any{"entity_id": "<from step 0>"}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["entity_id"] != "res-id-001" {
		t.Errorf("entity_id = %q, want resolved from results[].id fallback", resolved["entity_id"])
	}
}

// TestResolveDependentArgs_RealIDNotOverwritten ensures a concrete (non-placeholder)
// ID is preserved even when the prior result contains a different entity.
func TestResolveDependentArgs_RealIDNotOverwritten(t *testing.T) {
	t.Parallel()

	prior := `{"entities":[{"id":"prior-001"}],"count":1}`
	args := map[string]any{"start_entity_id": "explicit-123"}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["start_entity_id"] != "explicit-123" {
		t.Errorf("start_entity_id = %q, want explicit value preserved", resolved["start_entity_id"])
	}
}
