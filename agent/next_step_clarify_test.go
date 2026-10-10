package agent

import (
	"context"
	"strings"
	"testing"
)

// OGA-446: the clarify outcome + its reactive-only contract gate.

func TestRequestNextStep_ParsesClarify(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"two chillers match","clarify":{"question":"Which chiller?","kind":"disambiguation","options":[{"id":"a","label":"Carrier"},{"id":"b","label":"Trane"}],"pending_tool":"fm_create_work_order","partial_arguments":{"work_type":"inspection"}}}`},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "create wo", AllowClarification: true}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if d.Final || d.ToolName != "" {
		t.Fatalf("expected clarify, got final=%v tool=%q", d.Final, d.ToolName)
	}
	if d.Clarification == nil || d.Clarification.Question != "Which chiller?" {
		t.Fatalf("clarification not parsed: %+v", d.Clarification)
	}
	if d.Clarification.Kind != ClarifyKindDisambiguation || d.Clarification.PendingTool != "fm_create_work_order" {
		t.Errorf("clarify fields wrong: %+v", d.Clarification)
	}
	if len(d.Clarification.Options) != 2 {
		t.Errorf("options = %d, want 2", len(d.Clarification.Options))
	}
}

func TestRequestNextStep_FinalWinsOverClarify(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"done","final":true,"clarify":{"question":"ignored?"}}`},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q", AllowClarification: true}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if !d.Final || d.Clarification != nil {
		t.Errorf("final must win over clarify: final=%v clar=%+v", d.Final, d.Clarification)
	}
}

func TestRequestNextStep_ClarifyWinsOverAction(t *testing.T) {
	gw := &fakeGateway{chatResponses: []chatResponse{
		{content: `{"thought":"ambiguous","clarify":{"question":"Which one?"},"action":{"tool":"kg_create_entity","arguments":{}}}`},
	}}
	d, err := RequestNextStep(context.Background(), gw, NextStepRequest{Query: "q", AllowClarification: true}, DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("RequestNextStep: %v", err)
	}
	if d.Clarification == nil {
		t.Fatalf("clarify must win over action, got tool=%q final=%v", d.ToolName, d.Final)
	}
	if d.ToolName != "" {
		t.Errorf("tool should be empty when clarifying, got %q", d.ToolName)
	}
}

// Property 1 support: the clarify contract is present in the decision prompt
// ONLY when AllowClarification is true. When false the prompt is the pre-OGA-446
// two-outcome contract.
func TestRequestNextStep_ClarifyContractGatedByAllowClarification(t *testing.T) {
	const marker = `"clarify"`

	on := &fakeGateway{chatResponses: []chatResponse{{content: `{"thought":"t","final":true}`}}}
	if _, err := RequestNextStep(context.Background(), on, NextStepRequest{Query: "q", AllowClarification: true}, DefaultPlannerConfig()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on.chatCalls[0].Messages[0].Content, marker) {
		t.Error("AllowClarification=true: decision prompt should include the clarify contract")
	}

	off := &fakeGateway{chatResponses: []chatResponse{{content: `{"thought":"t","final":true}`}}}
	if _, err := RequestNextStep(context.Background(), off, NextStepRequest{Query: "q", AllowClarification: false}, DefaultPlannerConfig()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off.chatCalls[0].Messages[0].Content, marker) {
		t.Error("AllowClarification=false: decision prompt must NOT include the clarify contract (proactive purity)")
	}
}
