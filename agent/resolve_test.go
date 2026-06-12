package agent

import (
	"encoding/json"
	"testing"
)

// --- Property 1: known-tool fidelity (per-tool table) ---

func TestExtract_PerTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tool     string
		content  string
		wantID   string
		wantKind IDKind
	}{
		{
			name:     "kg_search results/entity_id",
			tool:     "kg_search",
			content:  `{"results":[{"entity_id":"ent-1","entity_type":"brick_Chiller"}]}`,
			wantID:   "ent-1",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_query_entities entities/id",
			tool:     "kg_query_entities",
			content:  `{"entities":[{"id":"ent-2","entity_type":"brick_AHU"}],"count":1}`,
			wantID:   "ent-2",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_get_entity single entity_id",
			tool:     "kg_get_entity",
			content:  `{"entity_id":"ent-3","entity_type":"brick_VAV"}`,
			wantID:   "ent-3",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_vector similar results/entity_id",
			tool:     "kg_vector",
			content:  `{"mode":"similar","results":[{"entity_id":"ent-4"}]}`,
			wantID:   "ent-4",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_vector cluster representative_entity",
			tool:     "kg_vector",
			content:  `{"mode":"cluster","clusters":[{"representative_entity":"ent-5","size":3}]}`,
			wantID:   "ent-5",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_reason nested path entity_id",
			tool:     "kg_reason",
			content:  `{"results":[{"matched_entities":["m-1"],"path":[{"entity_id":"path-1","hop":0}]}]}`,
			wantID:   "path-1",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_traverse nodes/id",
			tool:     "kg_traverse",
			content:  `{"start_entity_id":"s-1","nodes":[{"id":"node-1"},{"id":"node-2"}]}`,
			wantID:   "node-1",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_query_relationships endpoints",
			tool:     "kg_query_relationships",
			content:  `{"relationships":[{"id":"rel-1","source_entity_id":"src-1","target_entity_id":"tgt-1"}]}`,
			wantID:   "src-1",
			wantKind: IDKindEntity,
		},
		{
			name:     "kg_doc_search documents/id",
			tool:     "kg_doc_search",
			content:  `{"documents":[{"id":"doc-1","title":"SOP"}]}`,
			wantID:   "doc-1",
			wantKind: IDKindDocument,
		},
		{
			name:     "kg_doc_content passages/document_id",
			tool:     "kg_doc_content",
			content:  `{"passages":[{"document_id":"doc-2","document_name":"Manual"}]}`,
			wantID:   "doc-2",
			wantKind: IDKindDocument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ids := Extract(tt.content, lookupDescriptor(tt.tool))
			if len(ids) == 0 {
				t.Fatalf("Extract(%s) returned no identifiers", tt.tool)
			}
			first := firstOfKind(ids, tt.wantKind)
			if first == nil {
				t.Fatalf("no identifier of kind %s; got %+v", tt.wantKind, ids)
			}
			if first.Value != tt.wantID {
				t.Errorf("first %s id = %q, want %q", tt.wantKind, first.Value, tt.wantID)
			}
		})
	}
}

// --- The kg_reason silent-miss case, end to end ---

func TestResolveForTool_KgReason(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"matched_entities":["m-1"],"path":[{"entity_id":"reason-entity-1","hop":0}]}],"total_count":1}`
	args := map[string]any{"start_entity_id": "<from step 0>"}

	resolved := ResolveDependentArgsForTool(args, prior, "kg_reason")

	if resolved["start_entity_id"] != "reason-entity-1" {
		t.Errorf("start_entity_id = %q, want resolved from kg_reason path[].entity_id", resolved["start_entity_id"])
	}
}

// --- Property 3: kind safety ---

func TestResolveForTool_DocKindSafety(t *testing.T) {
	t.Parallel()

	prior := `{"passages":[{"document_id":"doc-9","document_name":"Manual"}]}`
	args := map[string]any{
		"document_id": "<from step 0>",
		"entity_id":   "<from step 0>",
	}

	resolved := ResolveDependentArgsForTool(args, prior, "kg_doc_content")

	if resolved["document_id"] != "doc-9" {
		t.Errorf("document_id = %q, want doc-9", resolved["document_id"])
	}
	// A document identifier must never fill an entity argument.
	if resolved["entity_id"] != "<from step 0>" {
		t.Errorf("entity_id = %q, want placeholder untouched (no entity id available)", resolved["entity_id"])
	}
}

func TestResolveForTool_EntityKindDoesNotFillDocument(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"entity_id":"ent-7"}]}`
	args := map[string]any{"document_id": "<from step 0>"}

	resolved := ResolveDependentArgsForTool(args, prior, "kg_search")

	if resolved["document_id"] != "<from step 0>" {
		t.Errorf("document_id = %q, want placeholder untouched (entity id must not fill document_id)", resolved["document_id"])
	}
}

// --- Requirement 4.4 / named placeholder for entity_type + label ---

func TestResolveForTool_EntityTypeAndLabel(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"entity_id":"ent-8","entity_type":"brick_Chiller","label":"CH-01"}]}`
	args := map[string]any{
		"entity_type": "<from step 0>",
		"label":       "<from step 0>",
	}

	resolved := ResolveDependentArgsForTool(args, prior, "kg_search")

	if resolved["entity_type"] != "brick_Chiller" {
		t.Errorf("entity_type = %q, want brick_Chiller", resolved["entity_type"])
	}
	if resolved["label"] != "CH-01" {
		t.Errorf("label = %q, want CH-01", resolved["label"])
	}
}

// --- Requirement 3: heuristic fallback (unknown tool) ---

func TestHeuristic_UnknownTool(t *testing.T) {
	t.Parallel()

	// Unknown custom kit tool emitting a conventional entities array.
	prior := `{"entities":[{"id":"custom-ent-1"}]}`
	args := map[string]any{"start_entity_id": "<from step 0>"}

	resolved := ResolveDependentArgsForTool(args, prior, "my_custom_kit_tool")

	if resolved["start_entity_id"] != "custom-ent-1" {
		t.Errorf("start_entity_id = %q, want heuristic resolution", resolved["start_entity_id"])
	}
}

func TestHeuristic_NestedReasonShapeWithoutToolName(t *testing.T) {
	t.Parallel()

	// No tool name → heuristic must still find the nested entity id.
	prior := `{"results":[{"path":[{"entity_id":"nested-1"}]}]}`
	args := map[string]any{"entity_id": "<from step 0>"}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["entity_id"] != "nested-1" {
		t.Errorf("entity_id = %q, want nested-1 via heuristic depth-2 scan", resolved["entity_id"])
	}
}

// --- Property 4: deny-list / no false positives ---

func TestHeuristic_DenyListNoResolution(t *testing.T) {
	t.Parallel()

	// Only tenant_id and audit fields are present — none are identifiers.
	prior := `{"tenant_id":"019e38e3-69b8-7b61-849b-5a78e816e244","created_by":"019e0000-0000-0000-0000-000000000000"}`
	args := map[string]any{"entity_id": "<from step 0>"}

	resolved := ResolveDependentArgs(args, prior)

	if resolved["entity_id"] != "<from step 0>" {
		t.Errorf("entity_id = %q, want placeholder untouched (deny-listed keys must not resolve)", resolved["entity_id"])
	}
}

// --- Property 2: no overwrite ---

func TestResolve_NoOverwriteConcrete(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"entity_id":"prior-ent"}]}`
	args := map[string]any{"entity_id": "explicit-value"}

	resolved := ResolveDependentArgsForTool(args, prior, "kg_search")

	if resolved["entity_id"] != "explicit-value" {
		t.Errorf("entity_id = %q, want concrete value preserved", resolved["entity_id"])
	}
}

// --- Property 6: heuristic determinism ---

func TestHeuristic_Determinism(t *testing.T) {
	t.Parallel()

	// Multiple candidate arrays present; the fixed priority order must pick the
	// same one every run regardless of map iteration order.
	prior := `{"clusters":[{"representative_entity":"c-1"}],"entities":[{"id":"e-1"}],"results":[{"entity_id":"r-1"}]}`

	var firstSeen string
	for i := 0; i < 50; i++ {
		ids := Extract(prior, nil)
		if len(ids) == 0 {
			t.Fatal("expected at least one identifier")
		}
		if i == 0 {
			firstSeen = ids[0].Value
			continue
		}
		if ids[0].Value != firstSeen {
			t.Fatalf("nondeterministic: run %d picked %q, first run picked %q", i, ids[0].Value, firstSeen)
		}
	}
	// "results" has the highest priority among the three present.
	if firstSeen != "r-1" {
		t.Errorf("heuristic picked %q, want r-1 (results has highest priority)", firstSeen)
	}
}

// --- Requirement 5.3/5.4 + always injects _prior_result ---

func TestResolve_InjectsPriorResult(t *testing.T) {
	t.Parallel()

	prior := `{"results":[{"entity_id":"x"}]}`
	resolved := ResolveDependentArgs(nil, prior)

	if resolved[priorResultKey] != prior {
		t.Errorf("_prior_result = %v, want raw prior content", resolved[priorResultKey])
	}
}

func TestResolve_EmptyAndMalformed(t *testing.T) {
	t.Parallel()

	// Empty content: args unchanged except _prior_result.
	got := ResolveDependentArgs(map[string]any{"entity_id": "<from step 0>"}, "")
	if got["entity_id"] != "<from step 0>" {
		t.Errorf("empty prior: entity_id = %q, want untouched", got["entity_id"])
	}

	// Malformed JSON: no panic, placeholder untouched.
	got = ResolveDependentArgs(map[string]any{"entity_id": "<from step 0>"}, "not json {{{")
	if got["entity_id"] != "<from step 0>" {
		t.Errorf("malformed prior: entity_id = %q, want untouched", got["entity_id"])
	}
}

// --- Property 5: panic-free over arbitrary JSON ---

func FuzzResolveDependentArgsForTool(f *testing.F) {
	seeds := []string{
		`{"results":[{"entity_id":"e1"}]}`,
		`{"entities":[{"id":"e2"}]}`,
		`{"results":[{"path":[{"entity_id":"p1"}]}]}`,
		`{"passages":[{"document_id":"d1"}]}`,
		`{}`,
		`[]`,
		`null`,
		`"a string"`,
		`{"nested":{"deep":{"id":"x"}}}`,
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add(s, "kg_search")
		f.Add(s, "")
		f.Add(s, "unknown_tool")
	}

	f.Fuzz(func(t *testing.T, content, tool string) {
		args := map[string]any{
			"entity_id":       "<from step 0>",
			"start_entity_id": "",
			"document_id":     "<from step 1>",
			"keep":            "concrete",
		}
		resolved := ResolveDependentArgsForTool(args, content, tool)

		// Always injects _prior_result.
		if resolved[priorResultKey] != content {
			t.Fatalf("_prior_result not injected for content %q", content)
		}
		// Never overwrites the concrete value.
		if resolved["keep"] != "concrete" {
			t.Fatalf("concrete value overwritten: %v", resolved["keep"])
		}
		// Resolved values, if any, must be valid JSON-marshalable.
		if _, err := json.Marshal(resolved); err != nil {
			t.Fatalf("resolved args not marshalable: %v", err)
		}
	})
}
