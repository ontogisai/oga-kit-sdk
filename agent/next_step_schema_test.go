package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSummarizeSchema verifies the compact argument summary: required fields
// first (marked *), then the rest, each with its type.
func TestSummarizeSchema(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"source_id": {"type": "string"},
			"metric": {"type": "string"},
			"mode": {"type": "string", "enum": ["anomaly","threshold","forecast"]},
			"from": {"type": "string"}
		},
		"required": ["mode", "from"]
	}`)

	got := summarizeSchema(schema)

	// Required fields are marked with *. An enum field renders its allowed
	// values so the model picks a valid one (OGA-431).
	if !strings.Contains(got, "mode(string=anomaly|threshold|forecast)*") {
		t.Errorf("expected required mode with enum values marked, got %q", got)
	}
	if !strings.Contains(got, "from(string)*") {
		t.Errorf("expected required from marked, got %q", got)
	}
	// Optional fields present with type, no marker.
	if !strings.Contains(got, "source_id(string)") {
		t.Errorf("expected source_id, got %q", got)
	}
	if strings.Contains(got, "source_id(string)*") {
		t.Errorf("source_id is optional, must not be marked required: %q", got)
	}
	// Required fields render before optional ones.
	if strings.Index(got, "mode") > strings.Index(got, "source_id") {
		t.Errorf("required fields should precede optional ones: %q", got)
	}
}

func TestSummarizeSchema_Empty(t *testing.T) {
	if got := summarizeSchema(nil); got != "" {
		t.Errorf("expected empty for nil schema, got %q", got)
	}
}

// TestSummarizeSchema_EnumValuesRendered verifies a constrained field surfaces
// its allowed values, so the planner picks a valid one instead of inventing a
// plausible value (e.g. mode "single" for an enum of range|multi) — OGA-431.
func TestSummarizeSchema_EnumValuesRendered(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"mode": {"type": "string", "enum": ["range","multi"]},
			"metric": {"type": "string"}
		},
		"required": ["mode"]
	}`)
	got := summarizeSchema(schema)
	if !strings.Contains(got, "mode(string=range|multi)*") {
		t.Errorf("expected mode enum values rendered, got %q", got)
	}
	if strings.Contains(got, "single") {
		t.Errorf("unexpected value in summary: %q", got)
	}
}

// TestSummarizeEnum_Bounds verifies the enum renderer caps long enums.
func TestSummarizeEnum_Bounds(t *testing.T) {
	many := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		many = append(many, i)
	}
	got := summarizeEnum(many)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation marker for a large enum, got %q", got)
	}
	// Mixed scalar types stringify without error.
	if got := summarizeEnum([]any{"a", 1, true}); got != "a|1|true" {
		t.Errorf("mixed enum = %q, want a|1|true", got)
	}
}

func TestSummarizeSchema_NoProperties_FallsBackToCompact(t *testing.T) {
	// A schema without a properties block falls back to compacted raw JSON.
	raw := json.RawMessage(`{ "type":  "string" }`)
	got := summarizeSchema(raw)
	if got != `{"type":"string"}` {
		t.Errorf("expected compacted raw, got %q", got)
	}
}

// TestRenderToolPalette_WithSchemas verifies tools render with description +
// argument summary, and that names-only tools (no matching schema) still appear.
func TestRenderToolPalette_WithSchemas(t *testing.T) {
	tools := []string{"kg_search", "kg_traverse"}
	schemas := []ToolSchema{
		{
			Name:        "kg_search",
			Description: "Hybrid entity search",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		},
		// A delegation-style entry: description, no input schema.
		{Name: "ask_knowledge_agent", Description: "Ask the platform Knowledge Agent"},
	}

	got := renderToolPalette(tools, schemas)

	if !strings.Contains(got, "kg_search: Hybrid entity search") {
		t.Errorf("expected kg_search description, got:\n%s", got)
	}
	if !strings.Contains(got, "query(string)*") {
		t.Errorf("expected kg_search args summary, got:\n%s", got)
	}
	// Names-only tool (no schema) still listed.
	if !strings.Contains(got, "kg_traverse") {
		t.Errorf("expected kg_traverse in palette, got:\n%s", got)
	}
	// Schema-only entry (delegation) appended to the palette.
	if !strings.Contains(got, "ask_knowledge_agent: Ask the platform Knowledge Agent") {
		t.Errorf("expected delegation entry, got:\n%s", got)
	}
}

func TestRenderToolPalette_NamesOnly(t *testing.T) {
	got := renderToolPalette([]string{"a", "b"}, nil)
	if !strings.Contains(got, "- a") || !strings.Contains(got, "- b") {
		t.Errorf("expected both names listed, got %q", got)
	}
}

func TestRenderToolPalette_Empty(t *testing.T) {
	if got := renderToolPalette(nil, nil); got != "" {
		t.Errorf("expected empty palette, got %q", got)
	}
}

// TestRequestNextStep_RendersSchemasInPrompt verifies the schemas reach the
// decision call's system prompt.
func TestRequestNextStep_RendersSchemasInPrompt(t *testing.T) {
	gw := &fakeGateway{
		chatResponses: []chatResponse{{content: `{"thought":"ok","final":true}`}},
	}

	_, err := RequestNextStep(t.Context(), gw, NextStepRequest{
		SystemPrompt: "You are a test agent.",
		Tools:        []string{"kg_search"},
		ToolSchemas: []ToolSchema{
			{Name: "kg_search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)},
		},
		Query: "find things",
	}, PlannerConfig{})
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}

	if len(gw.chatCalls) == 0 {
		t.Fatal("expected a chat completion call")
	}
	var capturedSystem string
	for _, m := range gw.chatCalls[0].Messages {
		if m.Role == "system" {
			capturedSystem = m.Content
		}
	}
	if !strings.Contains(capturedSystem, "query(string)*") {
		t.Errorf("decision prompt should carry the tool arg schema, got:\n%s", capturedSystem)
	}
}
