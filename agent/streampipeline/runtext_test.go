package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRunText_DrainsPipelineToText verifies the non-streaming RunText entry
// point exercises the same ReAct loop and returns the assembled answer +
// consolidated citations.
func TestRunText_DrainsPipelineToText(t *testing.T) {
	gw := &fakeGateway{
		tools: map[string]json.RawMessage{
			"kg_search": json.RawMessage(`{"results":[{"id":"e1","name":"Building A"}]}`),
		},
		streamChunks: []string{"Building A ", "is the answer."},
	}
	planner := &scriptedPlanner{
		steps:     []ToolPlanStep{{Name: "s0", ToolName: "kg_search", DependsOn: -1}},
		narrative: "Planning...",
	}

	text, citations, err := RunText(context.Background(), NewPipeline(),
		Deps{Gateway: gw, Config: DefaultConfig()},
		Input{Query: "what is building A?"},
		planner,
	)
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if !strings.Contains(text, "Building A is the answer.") {
		t.Errorf("assembled text = %q, want the streamed answer", text)
	}
	if len(citations) == 0 {
		t.Errorf("expected consolidated citations from the kg_search result")
	}
	if len(gw.callToolCalls) != 1 || gw.callToolCalls[0].Tool != "kg_search" {
		t.Errorf("expected kg_search executed once, got %v", gw.callToolCalls)
	}
}

// TestRunText_NilPipeline constructs a pipeline when nil is passed.
func TestRunText_NilPipeline(t *testing.T) {
	gw := &fakeGateway{streamChunks: []string{"hi"}}
	planner := &scriptedPlanner{} // no steps → plain answer
	text, _, err := RunText(context.Background(), nil,
		Deps{Gateway: gw, Config: DefaultConfig()},
		Input{Query: "hello"}, planner)
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if !strings.Contains(text, "hi") {
		t.Errorf("text = %q, want plain answer", text)
	}
}
