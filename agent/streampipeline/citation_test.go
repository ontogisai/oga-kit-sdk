package streampipeline

import (
	"strings"
	"testing"
)

func TestExtractEntityCitations(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantCount   int
		wantFirstID string
	}{
		{
			name:        "results array shape",
			content:     `{"results":[{"id":"e1","name":"Building A"},{"id":"e2","name":"Building B"}]}`,
			wantCount:   2,
			wantFirstID: "e1",
		},
		{
			name:        "uses entity_id when id missing",
			content:     `{"results":[{"entity_id":"e1","name":"Foo"}]}`,
			wantCount:   1,
			wantFirstID: "e1",
		},
		{
			name:        "single entity shape",
			content:     `{"entity_id":"e1","name":"Solo"}`,
			wantCount:   1,
			wantFirstID: "e1",
		},
		{
			name:      "empty results",
			content:   `{"results":[]}`,
			wantCount: 0,
		},
		{
			name:      "malformed JSON",
			content:   `{not json`,
			wantCount: 0,
		},
		{
			name:      "no entity ids in results",
			content:   `{"results":[{"name":"NoID"}]}`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractEntityCitations(tt.content)
			if len(got) != tt.wantCount {
				t.Fatalf("got %d citations, want %d (citations=%v)", len(got), tt.wantCount, got)
			}
			if tt.wantCount > 0 && got[0].ID != tt.wantFirstID {
				t.Errorf("first citation ID = %q, want %q", got[0].ID, tt.wantFirstID)
			}
		})
	}
}

func TestExtractEntityCitations_CapsAtMax(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"results":[`)
	for i := 0; i < 25; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"e`)
		sb.WriteString(string(rune('0' + (i % 10))))
		sb.WriteString(string(rune('0' + (i / 10))))
		sb.WriteString(`","name":"X"}`)
	}
	sb.WriteString(`]}`)

	got := ExtractEntityCitations(sb.String())
	if len(got) != MaxCitationsPerStep {
		t.Errorf("got %d citations, want exactly %d (the cap)", len(got), MaxCitationsPerStep)
	}
}

func TestExtractCitations_Spatial(t *testing.T) {
	result := &ToolStepResult{
		StepIndex: 0,
		ToolName:  "kg_search",
		Success:   true,
		Content:   `{"results":[]}`,
	}
	args := map[string]any{
		"h3_cells": []any{"872830829ffffff", "872830828ffffff"},
	}
	got := ExtractCitations(result, "kg_search", args)
	// Should have h3_cells citation + generic fallback (since results is empty array)
	hasSpatial := false
	for _, c := range got {
		if c.Type == "h3_cells" {
			hasSpatial = true
			if len(c.H3Cells) != 2 {
				t.Errorf("got %d h3 cells, want 2", len(c.H3Cells))
			}
		}
	}
	if !hasSpatial {
		t.Errorf("expected h3_cells citation; got %v", got)
	}
}

func TestExtractCitations_Temporal(t *testing.T) {
	result := &ToolStepResult{
		StepIndex: 1,
		ToolName:  "kg_traverse",
		Success:   true,
		Content:   `{"results":[]}`,
	}
	args := map[string]any{
		"valid_from": "2026-01-01T00:00:00Z",
		"valid_to":   "2026-06-01T00:00:00Z",
	}
	got := ExtractCitations(result, "kg_traverse", args)
	hasTemporal := false
	for _, c := range got {
		if c.Type == "time_range" {
			hasTemporal = true
			if c.ValidFrom != "2026-01-01T00:00:00Z" {
				t.Errorf("ValidFrom = %q", c.ValidFrom)
			}
		}
	}
	if !hasTemporal {
		t.Errorf("expected time_range citation; got %v", got)
	}
}

func TestExtractCitations_GenericFallback(t *testing.T) {
	result := &ToolStepResult{
		StepIndex: 2,
		ToolName:  "kg_doc_content",
		Success:   true,
		Content:   `not json content`,
	}
	got := ExtractCitations(result, "kg_doc_content", nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 generic citation; got %d (%v)", len(got), got)
	}
	if got[0].Type != "entity" {
		t.Errorf("expected type=entity, got %q", got[0].Type)
	}
	if got[0].ID != "tool:kg_doc_content:2" {
		t.Errorf("expected ID=tool:kg_doc_content:2, got %q", got[0].ID)
	}
}

func TestExtractCitations_NotSuccessfulOrEmpty(t *testing.T) {
	cases := []*ToolStepResult{
		nil,
		{Success: false, Content: "x"},
		{Success: true, Content: ""},
	}
	for i, r := range cases {
		got := ExtractCitations(r, "tool", nil)
		if got != nil {
			t.Errorf("case %d: expected nil, got %v", i, got)
		}
	}
}

// --- ExtractDocumentCitations (OGA-316) ---

func TestExtractDocumentCitations_Passages(t *testing.T) {
	content := `{"passages":[
		{"document_id":"doc-3bbaf7dc","document_name":"Water Leakage SOP","content":"step 1"},
		{"document_id":"doc-9ab12345","document_name":"Emergency Response Procedure","content":"step 2"}
	]}`

	got := ExtractDocumentCitations(content)

	if len(got) != 2 {
		t.Fatalf("expected 2 document citations, got %d", len(got))
	}
	if got[0].Type != "document" {
		t.Errorf("expected Type=document, got %q", got[0].Type)
	}
	if got[0].ID != "doc-3bbaf7dc" || got[0].Label != "Water Leakage SOP" {
		t.Errorf("first citation wrong: %+v", got[0])
	}
	if got[1].ID != "doc-9ab12345" || got[1].Label != "Emergency Response Procedure" {
		t.Errorf("second citation wrong: %+v", got[1])
	}
}

func TestExtractDocumentCitations_EmptyName_FallsBackToID(t *testing.T) {
	content := `{"passages":[{"document_id":"doc-3bbaf7dc","document_name":""}]}`

	got := ExtractDocumentCitations(content)

	if len(got) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(got))
	}
	if got[0].Label != "doc-3bbaf7dc" {
		t.Errorf("expected Label to fall back to document_id, got %q", got[0].Label)
	}
}

func TestExtractDocumentCitations_DeduplicatesByDocumentID(t *testing.T) {
	// Same document appears in 3 passages → 1 citation.
	content := `{"passages":[
		{"document_id":"doc-A","document_name":"Doc A"},
		{"document_id":"doc-A","document_name":"Doc A"},
		{"document_id":"doc-B","document_name":"Doc B"},
		{"document_id":"doc-A","document_name":"Doc A"}
	]}`

	got := ExtractDocumentCitations(content)

	if len(got) != 2 {
		t.Fatalf("expected 2 unique citations, got %d", len(got))
	}
	if got[0].ID != "doc-A" || got[1].ID != "doc-B" {
		t.Errorf("expected [doc-A, doc-B] in insertion order, got [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestExtractDocumentCitations_SkipsEmptyDocumentID(t *testing.T) {
	content := `{"passages":[
		{"document_id":"","document_name":"missing id"},
		{"document_id":"doc-real","document_name":"real"}
	]}`

	got := ExtractDocumentCitations(content)

	if len(got) != 1 {
		t.Fatalf("expected 1 citation (empty id skipped), got %d", len(got))
	}
	if got[0].ID != "doc-real" {
		t.Errorf("expected ID=doc-real, got %q", got[0].ID)
	}
}

func TestExtractDocumentCitations_EmptyPassages(t *testing.T) {
	got := ExtractDocumentCitations(`{"passages":[]}`)
	if len(got) != 0 {
		t.Errorf("expected 0 citations, got %d", len(got))
	}
}

func TestExtractDocumentCitations_NonPassageJSON_ReturnsNil(t *testing.T) {
	got := ExtractDocumentCitations(`{"results":[{"id":"e-1","name":"Entity"}]}`)
	if got != nil {
		t.Errorf("expected nil for entity-shaped JSON, got %d citations", len(got))
	}
}

func TestExtractDocumentCitations_InvalidJSON_ReturnsNil(t *testing.T) {
	got := ExtractDocumentCitations("not valid json")
	if got != nil {
		t.Errorf("expected nil for invalid JSON, got %d citations", len(got))
	}
}

func TestExtractDocumentCitations_CapsAtMaxCitationsPerStep(t *testing.T) {
	// Build 15 unique passages — should cap at MaxCitationsPerStep (10).
	var sb strings.Builder
	sb.WriteString(`{"passages":[`)
	for i := range 15 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"document_id":"doc-`)
		sb.WriteByte(byte('a' + i))
		sb.WriteString(`","document_name":"Doc"}`)
	}
	sb.WriteString(`]}`)

	got := ExtractDocumentCitations(sb.String())

	if len(got) != MaxCitationsPerStep {
		t.Errorf("expected cap at %d, got %d", MaxCitationsPerStep, len(got))
	}
}

// --- ExtractCitations integration with document path (OGA-316) ---

func TestExtractCitations_DocumentTool_EmitsDocumentCitations(t *testing.T) {
	result := &ToolStepResult{
		StepIndex: 1,
		ToolName:  "kg_doc_content",
		Success:   true,
		Content:   `{"passages":[{"document_id":"doc-water-leak","document_name":"Water Leak SOP"}]}`,
	}

	got := ExtractCitations(result, "kg_doc_content", nil)

	if len(got) == 0 {
		t.Fatal("expected at least one citation, got none")
	}

	var foundDoc bool
	for _, c := range got {
		if c.Type == "document" && c.ID == "doc-water-leak" {
			foundDoc = true
		}
		// Generic fallback must NOT fire when document extraction succeeds.
		if c.Type == "entity" && strings.HasPrefix(c.ID, "tool:") {
			t.Errorf("generic fallback chip should not fire when document extraction yields citations: %+v", c)
		}
	}
	if !foundDoc {
		t.Errorf("expected document citation for doc-water-leak, got: %+v", got)
	}
}

func TestExtractCitations_MixedEntityAndDocument(t *testing.T) {
	// A tool result with both entity and passage shapes coexisting.
	result := &ToolStepResult{
		StepIndex: 0,
		ToolName:  "kg_hybrid",
		Success:   true,
		Content: `{
			"results":[{"id":"e1","name":"Building A"}],
			"passages":[{"document_id":"doc-1","document_name":"Doc One"}]
		}`,
	}

	got := ExtractCitations(result, "kg_hybrid", nil)

	var foundEntity, foundDoc bool
	for _, c := range got {
		if c.Type == "entity" && c.ID == "e1" {
			foundEntity = true
		}
		if c.Type == "document" && c.ID == "doc-1" {
			foundDoc = true
		}
	}
	if !foundEntity {
		t.Errorf("expected entity citation, got: %+v", got)
	}
	if !foundDoc {
		t.Errorf("expected document citation, got: %+v", got)
	}
}

func TestExtractCitations_NonStructuredContent_StillFallsBackToGeneric(t *testing.T) {
	// Regression: when neither entity nor document extraction parses the
	// content, the fallback path must still fire.
	result := &ToolStepResult{
		StepIndex: 2,
		ToolName:  "kg_traverse",
		Success:   true,
		Content:   `{"some_other_shape":"value"}`,
	}

	got := ExtractCitations(result, "kg_traverse", nil)

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 fallback citation, got %d", len(got))
	}
	if got[0].Type != "entity" || got[0].ID != "tool:kg_traverse:2" {
		t.Errorf("expected fallback chip, got %+v", got[0])
	}
}
