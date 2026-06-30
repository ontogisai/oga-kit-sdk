package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// clarifyPlanner returns a clarification on the first turn, then Done.
type clarifyPlanner struct{ payload *agent.ClarificationPayload }

func (p *clarifyPlanner) Next(_ context.Context, st *PlanState) (*Decision, error) {
	if len(st.History) == 0 {
		return &Decision{Narrative: "need to ask", Clarification: p.payload}, nil
	}
	return &Decision{Done: true}, nil
}

// singleStepPlanner returns one tool step on turn 0, then Done.
type singleStepPlanner struct{ step ToolPlanStep }

func (p *singleStepPlanner) Next(_ context.Context, st *PlanState) (*Decision, error) {
	if len(st.History) == 0 {
		s := p.step
		return &Decision{Step: &s}, nil
	}
	return &Decision{Done: true}, nil
}

func findStatus(events []*agent.StreamEvent) *agent.StatusPayload {
	for _, e := range events {
		if e.Type == agent.EventTypeStatus {
			if sp, ok := e.Payload.(*agent.StatusPayload); ok {
				return sp
			}
		}
	}
	return nil
}

// Property 2: a clarify decision ends in input-required, emits the question as
// an artifact, and executes ZERO tools.
func TestPipeline_Clarify_EmitsInputRequired_NoTool(t *testing.T) {
	gw := &fakeGateway{tools: map[string]json.RawMessage{}}
	planner := &clarifyPlanner{payload: &agent.ClarificationPayload{
		Question:    "Which chiller — Carrier or Trane?",
		Kind:        agent.ClarifyKindDisambiguation,
		PendingTool: "fm_create_work_order",
	}}

	events := runPipelineForTest(t, gw, planner, Input{
		Query:   "create a work order for the basement chiller",
		Actor:   agent.EventActor{Type: "test", ID: "fm", DisplayName: "FM"},
		Persona: PlannerPersona{AllowClarification: true},
	})

	sp := findStatus(events)
	if sp == nil || sp.State != agent.TaskStateInputRequired {
		t.Fatalf("expected terminal input-required status, got %+v", sp)
	}
	if sp.Clarification == nil || sp.Clarification.Question == "" {
		t.Fatalf("input-required must carry the clarification payload, got %+v", sp.Clarification)
	}
	if len(gw.callToolCalls) != 0 {
		t.Errorf("no tool may execute on a clarify turn, got %v", gw.callToolCalls)
	}
	// The question must also be delivered as agent text (channel-agnostic).
	var sawArtifact bool
	for _, e := range events {
		if e.Type == agent.EventTypeArtifact {
			sawArtifact = true
		}
	}
	if !sawArtifact {
		t.Error("expected the question to be emitted as a task/artifact")
	}
}

// Property 3: on the reactive path a mutating tool call without a matching
// PendingConfirmation is intercepted into a confirmation turn — the write does
// NOT happen.
func TestPipeline_ConfirmBeforeWrite_InterceptsMutation(t *testing.T) {
	gw := &fakeGateway{tools: map[string]json.RawMessage{
		"kg_create_entity": json.RawMessage(`{"created":true}`),
	}}
	planner := &singleStepPlanner{step: ToolPlanStep{Name: "create", ToolName: "kg_create_entity", DependsOn: -1}}

	events := runPipelineForTest(t, gw, planner, Input{
		Query:   "create it",
		Actor:   agent.EventActor{Type: "test", ID: "fm", DisplayName: "FM"},
		Persona: PlannerPersona{AllowClarification: true},
	})

	sp := findStatus(events)
	if sp == nil || sp.State != agent.TaskStateInputRequired {
		t.Fatalf("mutating call without confirmation must yield input-required, got %+v", sp)
	}
	if sp.Clarification == nil || sp.Clarification.Kind != agent.ClarifyKindConfirmation {
		t.Fatalf("expected a confirmation clarification, got %+v", sp.Clarification)
	}
	if len(gw.callToolCalls) != 0 {
		t.Errorf("the write must NOT execute before confirmation, got %v", gw.callToolCalls)
	}
}

// With a matching PendingConfirmation injected (the resume turn), the mutating
// tool executes.
func TestPipeline_ConfirmBeforeWrite_ExecutesAfterConfirmation(t *testing.T) {
	gw := &fakeGateway{
		tools:        map[string]json.RawMessage{"kg_create_entity": json.RawMessage(`{"created":true}`)},
		streamChunks: []string{"done"},
	}
	planner := &singleStepPlanner{step: ToolPlanStep{Name: "create", ToolName: "kg_create_entity", DependsOn: -1}}

	events := runPipelineForTest(t, gw, planner, Input{
		Query:   "yes",
		Actor:   agent.EventActor{Type: "test", ID: "fm", DisplayName: "FM"},
		Persona: PlannerPersona{AllowClarification: true},
		PendingConfirmation: &agent.ClarificationPayload{
			Kind:        agent.ClarifyKindConfirmation,
			PendingTool: "kg_create_entity",
		},
	})

	if len(gw.callToolCalls) != 1 || gw.callToolCalls[0].Tool != "kg_create_entity" {
		t.Fatalf("expected the confirmed write to execute, got %v", gw.callToolCalls)
	}
	sp := findStatus(events)
	if sp == nil || sp.State != agent.TaskStateCompleted {
		t.Errorf("a confirmed resume turn should complete normally, got %+v", sp)
	}
}

// Property 1 support: the proactive path (AllowClarification=false) does not
// intercept a mutating call — confirm-before-write is reactive-only.
func TestPipeline_ProactivePath_NoConfirmInterception(t *testing.T) {
	gw := &fakeGateway{
		tools:        map[string]json.RawMessage{"kg_create_entity": json.RawMessage(`{"created":true}`)},
		streamChunks: []string{"ok"},
	}
	planner := &singleStepPlanner{step: ToolPlanStep{Name: "create", ToolName: "kg_create_entity", DependsOn: -1}}

	events := runPipelineForTest(t, gw, planner, Input{
		Query:   "proactive",
		Actor:   agent.EventActor{Type: "test", ID: "fm", DisplayName: "FM"},
		Persona: PlannerPersona{AllowClarification: false}, // proactive
	})

	if len(gw.callToolCalls) != 1 {
		t.Errorf("proactive path must not intercept; expected the tool to run, got %v", gw.callToolCalls)
	}
	sp := findStatus(events)
	if sp == nil || sp.State == agent.TaskStateInputRequired {
		t.Errorf("proactive path must never emit input-required, got %+v", sp)
	}
}

// Regression (OGA-446 review): a DISAMBIGUATION token whose pending_tool is the
// same write tool must NOT authorize the write on the resume turn — only an
// explicit confirmation does. Otherwise confirm-before-write is bypassed in the
// common case (the disambiguation pause names the same tool it will write with).
func TestPipeline_ConfirmBeforeWrite_DisambiguationDoesNotAuthorize(t *testing.T) {
	gw := &fakeGateway{
		tools:        map[string]json.RawMessage{"kg_create_entity": json.RawMessage(`{"created":true}`)},
		streamChunks: []string{"ok"},
	}
	planner := &singleStepPlanner{step: ToolPlanStep{Name: "create", ToolName: "kg_create_entity", DependsOn: -1}}

	events := runPipelineForTest(t, gw, planner, Input{
		Query:   "the Carrier one",
		Actor:   agent.EventActor{Type: "test", ID: "fm", DisplayName: "FM"},
		Persona: PlannerPersona{AllowClarification: true},
		PendingConfirmation: &agent.ClarificationPayload{
			Kind:        agent.ClarifyKindDisambiguation, // NOT a confirmation
			PendingTool: "kg_create_entity",
		},
	})

	if len(gw.callToolCalls) != 0 {
		t.Errorf("a disambiguation token must NOT authorize the write, got %v", gw.callToolCalls)
	}
	sp := findStatus(events)
	if sp == nil || sp.State != agent.TaskStateInputRequired || sp.Clarification == nil ||
		sp.Clarification.Kind != agent.ClarifyKindConfirmation {
		t.Fatalf("expected a forced confirmation turn, got %+v", sp)
	}
}

func TestResumeSeedFacts(t *testing.T) {
	if got := resumeSeedFacts(nil); got != "" {
		t.Errorf("nil token → empty seed, got %q", got)
	}
	got := resumeSeedFacts(&agent.ClarificationPayload{
		Question:         "Which chiller?",
		PendingTool:      "fm_create_work_order",
		PartialArguments: map[string]any{"work_type": "inspection"},
	})
	for _, want := range []string{"Which chiller?", "fm_create_work_order", "work_type", "inspection"} {
		if !strings.Contains(got, want) {
			t.Errorf("seed missing %q:\n%s", want, got)
		}
	}
}

func TestToolMutates(t *testing.T) {
	tt := true
	ff := false
	schemas := map[string]agent.ToolSchema{
		"explicit_write": {Name: "explicit_write", Mutates: &tt},
		"explicit_read":  {Name: "explicit_read", Mutates: &ff},
	}
	cases := map[string]bool{
		"explicit_write":       true,
		"explicit_read":        false, // explicit flag wins over any name hint
		"fm_create_work_order": true,  // heuristic
		"kg_update_entity":     true,
		"kg_delete_entity":     true,
		"kg_search":            false,
		"kg_get_entity":        false,
		"kg_query_entities":    false,
	}
	for name, want := range cases {
		if got := toolMutates(schemas, name); got != want {
			t.Errorf("toolMutates(%q) = %v, want %v", name, got, want)
		}
	}
}
