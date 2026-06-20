package streampipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// fixedStepPlanner always proposes the SAME (tool, args) regardless of turn —
// used to exercise the no-progress duplicate-action guard (Property 3).
type fixedStepPlanner struct {
	step  ToolPlanStep
	calls int
}

func (p *fixedStepPlanner) Next(_ context.Context, _ *PlanState) (*Decision, error) {
	p.calls++
	s := p.step
	return &Decision{Step: &s}, nil
}

// distinctStepPlanner proposes a distinct tool each turn (keyed off observation
// count), proving the loop threads History and exercising the consecutive
// empty/failed guard when every observation is unproductive.
type distinctStepPlanner struct{ calls int }

func (p *distinctStepPlanner) Next(_ context.Context, st *PlanState) (*Decision, error) {
	p.calls++
	return &Decision{Step: &ToolPlanStep{
		ToolName:  fmt.Sprintf("kg_step_%d", len(st.History)),
		DependsOn: -1,
	}}, nil
}

// untilSuccessPlanner keeps proposing tools until it OBSERVES a successful
// result, then finalizes — proving decisions are grounded in observations
// (Property 1).
type untilSuccessPlanner struct{ calls int }

func (p *untilSuccessPlanner) Next(_ context.Context, st *PlanState) (*Decision, error) {
	p.calls++
	for _, r := range st.History {
		if r.Success {
			return &Decision{Done: true, Narrative: "Got a result; answering."}, nil
		}
	}
	return &Decision{Step: &ToolPlanStep{
		ToolName:  fmt.Sprintf("kg_try_%d", len(st.History)),
		DependsOn: -1,
	}}, nil
}

func countTypes(events []*agent.StreamEvent, typ agent.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// Property 3 — duplicate-action no-progress guard: a planner that repeats the
// same (tool,args) is stopped after the first execution, well before MaxSteps.
func TestReactLoop_NoProgress_DuplicateAction(t *testing.T) {
	gw := &fakeGateway{
		tools:        map[string]json.RawMessage{"kg_search": json.RawMessage(`{"results":[{"id":"e1"}]}`)},
		streamChunks: []string{"done"},
	}
	planner := &fixedStepPlanner{step: ToolPlanStep{ToolName: "kg_search", DependsOn: -1, Arguments: map[string]any{"q": "x"}}}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})

	if got := len(gw.callToolCalls); got != 1 {
		t.Errorf("expected exactly 1 tool execution before the duplicate guard stops the loop, got %d", got)
	}
	if planner.calls > 3 {
		t.Errorf("planner invoked %d times — duplicate guard should stop quickly (well under MaxSteps)", planner.calls)
	}
	if countTypes(events, agent.EventTypeStatus) == 0 {
		t.Error("expected a terminal status event")
	}
}

// Property 3 — consecutive unproductive observations stop the loop. Every tool
// fails; with NoProgressLimit=2 the loop halts after 2 failed observations.
func TestReactLoop_NoProgress_ConsecutiveFailures(t *testing.T) {
	gw := &fakeGateway{
		// Any tool name fails (default branch returns {"results":[]}, so use
		// an error matcher via a fallthrough: configure no tools + force error).
		toolErrors:   map[string]error{},
		streamChunks: []string{"done"},
	}
	// Make every call fail by routing through toolErrors dynamically: use a
	// completionFn-free fake; instead set tools to empty and rely on a custom
	// error. Simplest: pre-register errors for the tools the planner will emit.
	for i := 0; i < 6; i++ {
		gw.toolErrors[fmt.Sprintf("kg_step_%d", i)] = errors.New("backend down")
	}
	planner := &distinctStepPlanner{}

	_ = runPipelineForTest(t, gw, planner, Input{Query: "q"})

	if got := len(gw.callToolCalls); got != 2 {
		t.Errorf("expected loop to stop after 2 consecutive failures (NoProgressLimit), executed %d", got)
	}
}

// Property 1 — observation-grounded: the planner finalizes only after it
// observes a successful tool result. Proves History is threaded each turn.
func TestReactLoop_ObservationGrounded_FinalizesOnSuccess(t *testing.T) {
	gw := &fakeGateway{
		// kg_try_0 fails, kg_try_1 succeeds → planner should stop after turn 1.
		toolErrors:   map[string]error{"kg_try_0": errors.New("miss")},
		tools:        map[string]json.RawMessage{"kg_try_1": json.RawMessage(`{"results":[{"id":"hit"}]}`)},
		streamChunks: []string{"answer"},
	}
	planner := &untilSuccessPlanner{}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})

	if got := len(gw.callToolCalls); got != 2 {
		t.Errorf("expected 2 tool executions (miss then hit), got %d", got)
	}
	// Must complete (the success observation drove the finalize decision).
	completed := false
	for _, e := range events {
		if e.Type == agent.EventTypeStatus {
			if p, ok := e.Payload.(*agent.StatusPayload); ok && p.State == agent.TaskStateCompleted {
				completed = true
			}
		}
	}
	if !completed {
		t.Error("expected task/status{completed} after the planner observed a successful result")
	}
}

// Multi-turn evolving plan: a 2-step scripted run emits an incremental plan
// (re-emitted as it grows) and executes both tools in order.
func TestReactLoop_MultiTurn_EvolvingPlan(t *testing.T) {
	gw := &fakeGateway{
		tools: map[string]json.RawMessage{
			"kg_search":   json.RawMessage(`{"results":[{"id":"e1"}]}`),
			"kg_traverse": json.RawMessage(`{"nodes":[{"id":"e2"}]}`),
		},
		streamChunks: []string{"final"},
	}
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "s0", ToolName: "kg_search", DependsOn: -1},
			{Name: "s1", ToolName: "kg_traverse", DependsOn: -1},
		},
		narrative: "Planning...",
	}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})

	if got := len(gw.callToolCalls); got != 2 {
		t.Fatalf("expected 2 tool executions, got %d", got)
	}
	if gw.callToolCalls[0].Tool != "kg_search" || gw.callToolCalls[1].Tool != "kg_traverse" {
		t.Errorf("tools executed out of order: %v", gw.callToolCalls)
	}
	// The plan is re-emitted as it grows (>= 2 plan events: one per decided step).
	if got := countTypes(events, agent.EventTypePlan); got < 2 {
		t.Errorf("expected the evolving plan to be re-emitted per turn (>=2), got %d", got)
	}
	// A reasoning ("Thought") precedes tool calls; assembly adds one more.
	if countTypes(events, agent.EventTypeReasoning) < 1 {
		t.Error("expected at least one reasoning event")
	}
}
