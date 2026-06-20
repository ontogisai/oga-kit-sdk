package agent

import (
	"context"
	"strings"
	"testing"
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
