package streampipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// fakeGateway is a minimal gatewayClient stub for tests.
type fakeGateway struct {
	tools           map[string]json.RawMessage
	toolErrors      map[string]error
	completion      string
	completionErr   error
	completionFn    func(ctx context.Context, req *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error)
	streamChunks    []string
	streamErr       error
	callToolCalls   []callToolCall
	completionCalls int
}

type callToolCall struct {
	Tool   string
	Params any
}

func (f *fakeGateway) CallTool(_ context.Context, tool string, params any) (json.RawMessage, error) {
	f.callToolCalls = append(f.callToolCalls, callToolCall{Tool: tool, Params: params})
	if err, ok := f.toolErrors[tool]; ok {
		return nil, err
	}
	if raw, ok := f.tools[tool]; ok {
		return raw, nil
	}
	return json.RawMessage(`{"results":[]}`), nil
}

func (f *fakeGateway) ChatCompletion(ctx context.Context, req *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	f.completionCalls++
	if f.completionFn != nil {
		return f.completionFn(ctx, req)
	}
	if f.completionErr != nil {
		return nil, f.completionErr
	}
	return &gateway.ChatCompletionResponse{
		Choices: []gateway.ChatChoice{
			{
				Message: gateway.ChatMessage{Role: "assistant", Content: f.completion},
			},
		},
	}, nil
}

func (f *fakeGateway) ChatCompletionStream(_ context.Context, _ *gateway.ChatCompletionRequest) (<-chan *gateway.ChatChunk, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	if len(f.streamChunks) == 0 {
		return nil, errors.New("no stream chunks configured")
	}
	ch := make(chan *gateway.ChatChunk, len(f.streamChunks))
	for _, chunk := range f.streamChunks {
		ch <- &gateway.ChatChunk{
			Choices: []gateway.ChatChunkChoice{
				{Delta: gateway.ChatDelta{Content: chunk}},
			},
		}
	}
	close(ch)
	return ch, nil
}

// fakePlanner returns a fixed plan + narrative.
type fakePlanner struct {
	plan      *ToolPlan
	narrative *PlanNarrative
	err       error
}

func (p *fakePlanner) Plan(_ context.Context, _ string, _ []string) (*ToolPlan, *PlanNarrative, error) {
	return p.plan, p.narrative, p.err
}

// runPipelineForTest is a helper that runs Pipeline.Run with a fake gateway
// (via the streampipeline-internal gatewayClient interface) and collects events.
//
// Pipeline.Run takes a *gateway.PlatformGatewayClient on its public Deps,
// but the internal executor uses the gatewayClient interface — so we route
// the fake through a small shim: build a Pipeline with Deps containing
// nil gateway, then patch the exec function. Easier: temporarily expose a
// test-only Run path that takes the interface.
//
// We accomplish this by adding RunWithClient below for testability.
func runPipelineForTest(t *testing.T, gw gatewayClient, planner StreamPlanner, input Input) []*agent.StreamEvent {
	t.Helper()
	pipeline := NewPipeline()
	events := make(chan *agent.StreamEvent, 64)
	deps := Deps{Config: DefaultConfig()}

	collected := make([]*agent.StreamEvent, 0, 32)
	done := make(chan struct{})
	go func() {
		for evt := range events {
			collected = append(collected, evt)
		}
		close(done)
	}()

	_ = pipeline.runInternal(context.Background(), deps, input, planner, events, gw)
	close(events)
	<-done

	return collected
}

func TestPipeline_PlanWithSteps(t *testing.T) {
	gw := &fakeGateway{
		tools: map[string]json.RawMessage{
			"kg_search": json.RawMessage(`{"results":[{"id":"e1","name":"Building A"}]}`),
		},
		streamChunks: []string{"Hello", " world"},
	}
	planner := &fakePlanner{
		plan: &ToolPlan{Steps: []ToolPlanStep{
			{Name: "search", ToolName: "kg_search", DependsOn: -1},
		}},
		narrative: &PlanNarrative{Text: "Planning..."},
	}

	events := runPipelineForTest(t, gw, planner, Input{
		Query: "find building",
		Actor: agent.EventActor{Type: "test", ID: "test-agent", DisplayName: "Test"},
	})

	types := eventTypes(events)
	want := []agent.EventType{
		agent.EventTypeReasoning, // narrative
		agent.EventTypePlan,
		agent.EventTypeToolCall,
		agent.EventTypeToolResult,
		agent.EventTypeCitation,  // per-step
		agent.EventTypeReasoning, // "Assembling..."
		agent.EventTypeArtifact,
		agent.EventTypeArtifact, // 2 chunks
		agent.EventTypeCitation, // consolidated
		agent.EventTypeStatus,
	}
	if !sequenceMatches(types, want) {
		t.Errorf("event sequence mismatch.\n got: %v\nwant: %v", types, want)
	}

	// Verify tool was actually called
	if len(gw.callToolCalls) != 1 || gw.callToolCalls[0].Tool != "kg_search" {
		t.Errorf("expected kg_search call; got %v", gw.callToolCalls)
	}
}

func TestPipeline_RequiredStepFailure_StopsPipeline(t *testing.T) {
	gw := &fakeGateway{
		toolErrors: map[string]error{
			"kg_must_succeed": errors.New("backend unavailable"),
		},
	}
	planner := &fakePlanner{
		plan: &ToolPlan{Steps: []ToolPlanStep{
			{Name: "first", ToolName: "kg_must_succeed", DependsOn: -1, Required: true},
			{Name: "second", ToolName: "kg_after", DependsOn: -1},
		}},
		narrative: &PlanNarrative{Text: "Planning..."},
	}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})
	types := eventTypes(events)

	// Should see: reasoning, plan, tool_call, tool_result, status(failed).
	// MUST NOT see a second tool_call.
	toolCallCount := 0
	hasFailed := false
	for _, evt := range events {
		if evt.Type == agent.EventTypeToolCall {
			toolCallCount++
		}
		if evt.Type == agent.EventTypeStatus {
			if p, ok := evt.Payload.(*agent.StatusPayload); ok && p.State == agent.TaskStateFailed {
				hasFailed = true
			}
		}
	}
	if toolCallCount != 1 {
		t.Errorf("expected exactly 1 tool_call (required step), got %d (events: %v)", toolCallCount, types)
	}
	if !hasFailed {
		t.Errorf("expected task/status{failed}, got events: %v", types)
	}
}

func TestPipeline_NonRequiredStepFailure_Continues(t *testing.T) {
	gw := &fakeGateway{
		toolErrors: map[string]error{
			"kg_optional": errors.New("transient"),
		},
		tools: map[string]json.RawMessage{
			"kg_after": json.RawMessage(`{"results":[{"id":"x","name":"X"}]}`),
		},
		streamChunks: []string{"answer"},
	}
	planner := &fakePlanner{
		plan: &ToolPlan{Steps: []ToolPlanStep{
			{Name: "optional", ToolName: "kg_optional", DependsOn: -1, Required: false},
			{Name: "ok", ToolName: "kg_after", DependsOn: -1},
		}},
		narrative: &PlanNarrative{Text: "Planning..."},
	}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})

	completedSeen := false
	for _, evt := range events {
		if evt.Type == agent.EventTypeStatus {
			if p, ok := evt.Payload.(*agent.StatusPayload); ok && p.State == agent.TaskStateCompleted {
				completedSeen = true
			}
		}
	}
	if !completedSeen {
		t.Errorf("expected task/status{completed} despite non-required step failure")
	}
	if len(gw.callToolCalls) != 2 {
		t.Errorf("expected both tools called; got %d", len(gw.callToolCalls))
	}
}

func TestPipeline_ConditionalSkip_EmitsToolCallSkipped(t *testing.T) {
	gw := &fakeGateway{
		tools:        map[string]json.RawMessage{},
		streamChunks: []string{"answer"},
	}
	planner := &fakePlanner{
		plan: &ToolPlan{Steps: []ToolPlanStep{
			{Name: "skipped", ToolName: "kg_skipped", DependsOn: -1, Condition: "false"},
		}},
		narrative: &PlanNarrative{Text: "Planning..."},
	}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})

	hasSkippedToolCall := false
	hasToolResult := false
	for _, evt := range events {
		if evt.Type == agent.EventTypeToolCall {
			if p, ok := evt.Payload.(*agent.ToolCallPayload); ok && p.Skipped {
				hasSkippedToolCall = true
			}
		}
		if evt.Type == agent.EventTypeToolResult {
			hasToolResult = true
		}
	}

	if !hasSkippedToolCall {
		t.Errorf("expected tool_call with Skipped=true")
	}
	if hasToolResult {
		t.Errorf("expected NO tool_result for skipped step")
	}
	if len(gw.callToolCalls) != 0 {
		t.Errorf("expected gateway not called for skipped step; got %d calls", len(gw.callToolCalls))
	}
}

func TestPipeline_EmptyPlan_PlainAnswer(t *testing.T) {
	gw := &fakeGateway{streamChunks: []string{"hi there"}}
	planner := &fakePlanner{
		plan:      &ToolPlan{},
		narrative: &PlanNarrative{Text: "No tools..."},
	}

	events := runPipelineForTest(t, gw, planner, Input{Query: "hello"})

	hasArtifact := false
	hasPlan := false
	for _, evt := range events {
		if evt.Type == agent.EventTypeArtifact {
			hasArtifact = true
		}
		if evt.Type == agent.EventTypePlan {
			hasPlan = true
		}
	}
	if !hasArtifact {
		t.Errorf("expected at least one artifact event")
	}
	if hasPlan {
		t.Errorf("expected NO plan event for empty plan")
	}
}

func TestPipeline_PlannerError_EmitsFailed(t *testing.T) {
	gw := &fakeGateway{}
	planner := &fakePlanner{err: errors.New("planner exploded")}

	events := runPipelineForTest(t, gw, planner, Input{Query: "q"})

	hasFailed := false
	for _, evt := range events {
		if evt.Type == agent.EventTypeStatus {
			if p, ok := evt.Payload.(*agent.StatusPayload); ok && p.State == agent.TaskStateFailed {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Errorf("expected task/status{failed} for planner error")
	}
}

// --- helpers ---

func eventTypes(events []*agent.StreamEvent) []agent.EventType {
	types := make([]agent.EventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

// sequenceMatches checks that got starts with want's prefix in the same order.
// Allows extra events at end (so a longer run is fine if the prefix matches).
func sequenceMatches(got, want []agent.EventType) bool {
	if len(got) < len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
