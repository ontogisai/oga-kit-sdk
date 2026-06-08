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
