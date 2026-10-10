package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

func TestRequestNextStep_ParsesAction(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"search for the building first","action":{"tool":"kg_search","arguments":{"query":"Tower 2"}}}`},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{
		SystemPrompt: "You are FM ops.",
		Tools:        []string{"kg_search", "kg_traverse"},
		Query:        "what is in Tower 2?",
	}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if d.Final {
		t.Fatal("expected an action, got final")
	}
	if d.ToolName != "kg_search" {
		t.Errorf("tool = %q, want kg_search", d.ToolName)
	}
	if d.Arguments["query"] != "Tower 2" {
		t.Errorf("args.query = %v, want Tower 2", d.Arguments["query"])
	}
	if d.Thought == "" {
		t.Error("expected a non-empty thought")
	}
}

func TestRequestNextStep_ParsesFinal(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: "```json\n{\"thought\":\"I have enough\",\"final\":true}\n```"},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q"}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if !d.Final {
		t.Errorf("expected final, got action %q", d.ToolName)
	}
}

func TestRequestNextStep_NoActionMeansFinal(t *testing.T) {
	// A reply with neither final nor an action tool is treated as final.
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"nothing to do"}`},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q"}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if !d.Final {
		t.Error("expected final when no action tool is provided")
	}
}

func TestRequestNextStep_CorrectiveRetryOnProse(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: "Sure! I think we should search the knowledge graph."}, // prose → parse fail
		{content: `{"thought":"ok","action":{"tool":"kg_search","arguments":{}}}`},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q"}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if d.ToolName != "kg_search" {
		t.Errorf("tool = %q, want kg_search after corrective retry", d.ToolName)
	}
	if len(gw.chatCalls) != 2 {
		t.Errorf("expected 2 chat calls (initial + corrective), got %d", len(gw.chatCalls))
	}
}

func TestRequestNextStep_RendersToolsHintsAndTranscript(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"t","final":true}`},
	}}
	_, err := RequestNextStep(context.Background(), gw, NextStepRequest{
		SystemPrompt: "PERSONA",
		Tools:        []string{"kg_search"},
		Query:        "the question",
		SeedFacts:    "entity_id: chiller-1",
		Hints:        []GroundingHint{{Tool: "kg_doc_content", Rationale: "fetch SOP", StronglyAdvised: true}},
		History: []NextStepObservation{
			{ToolName: "kg_search", Success: true, Content: `{"results":[]}`},
		},
	}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if len(gw.chatCalls) != 1 {
		t.Fatalf("expected 1 chat call, got %d", len(gw.chatCalls))
	}
	msgs := gw.chatCalls[0].Messages
	sys := msgs[0].Content
	user := msgs[1].Content
	for _, want := range []string{"PERSONA", "kg_search", "kg_doc_content", "fetch SOP", "strongly advised"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q\n%s", want, sys)
		}
	}
	for _, want := range []string{"entity_id: chiller-1", "the question", "Observations so far", "kg_search"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n%s", want, user)
		}
	}
}

// TestRequestNextStep_CapturesUsage verifies the decision call surfaces token
// usage from the gateway response (OGA-420 Gap 2).
func TestRequestNextStep_CapturesUsage(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{
			content: `{"thought":"look it up","action":{"tool":"kg_search","arguments":{"q":"x"}}}`,
			usage:   &gateway.Usage{PromptTokens: 200, CompletionTokens: 40, TotalTokens: 240},
		},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q"}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if !d.UsageAvailable {
		t.Fatal("expected UsageAvailable=true")
	}
	if d.Usage.PromptTokens != 200 || d.Usage.CompletionTokens != 40 || d.Usage.TotalTokens != 240 {
		t.Errorf("usage = %+v, want {200,40,240}", d.Usage)
	}
}

// TestRequestNextStep_UsageSumsCorrectiveRetry verifies the decision usage sums
// the initial (unparseable) completion and the corrective retry (OGA-420).
func TestRequestNextStep_UsageSumsCorrectiveRetry(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `not json`, usage: &gateway.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}},
		{content: `{"thought":"ok","final":true}`, usage: &gateway.Usage{PromptTokens: 150, CompletionTokens: 5, TotalTokens: 155}},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q"}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if !d.Final {
		t.Error("expected Final after corrective retry")
	}
	if d.Usage.TotalTokens != 265 {
		t.Errorf("summed usage total = %d, want 265 (110+155)", d.Usage.TotalTokens)
	}
}

// TestRequestNextStep_NoUsageLabelledUnavailable verifies that when the proxy
// reports no usage, the decision is labelled unavailable with zero counts.
func TestRequestNextStep_NoUsageLabelledUnavailable(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"ok","final":true}`}, // no usage
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q"}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if d.UsageAvailable {
		t.Error("expected UsageAvailable=false when proxy reported none")
	}
	if d.Usage.TotalTokens != 0 {
		t.Errorf("usage should be zero, got %+v", d.Usage)
	}
}
