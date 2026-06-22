package streampipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// fakeGateway is a minimal PlatformAccess stub for tests.
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

	// OGA-420 additive test hooks (default nil → no behavior change):
	//   completionUsage — attached to every non-streaming ChatCompletion response.
	//   streamUsage     — emitted as a final usage-bearing chunk by ChatCompletionStream.
	//   chatReqs        — records every request so tests can assert model/temperature passthrough.
	completionUsage *gateway.Usage
	streamUsage     *gateway.Usage
	chatReqs        []*gateway.ChatCompletionRequest

	// streamChunksSeq, when non-empty, supplies a DISTINCT chunk-set per
	// ChatCompletionStream call (clamped to the last set once exhausted). Lets a
	// test return bad JSON on the first assembly and good JSON on the retry
	// (OGA-423 Gap 2A). When nil, streamChunks is used for every call.
	streamChunksSeq [][]string
	streamCallIdx   int

	// delegateRaw holds JSON-encoded sub-agent StreamEvents yielded by
	// InvokeAgentStream (OGA-419 G3 delegation tests). delegateErr, when set,
	// is returned instead. delegateCalls records the agent names invoked.
	delegateRaw   []string
	delegateErr   error
	delegateCalls []string
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
	f.chatReqs = append(f.chatReqs, req)
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
		Usage: f.completionUsage,
	}, nil
}

func (f *fakeGateway) ChatCompletionStream(_ context.Context, req *gateway.ChatCompletionRequest) (<-chan *gateway.ChatChunk, error) {
	f.chatReqs = append(f.chatReqs, req)
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	chunks := f.streamChunks
	if len(f.streamChunksSeq) > 0 {
		i := f.streamCallIdx
		if i >= len(f.streamChunksSeq) {
			i = len(f.streamChunksSeq) - 1
		}
		chunks = f.streamChunksSeq[i]
		f.streamCallIdx++
	}
	if len(chunks) == 0 {
		return nil, errors.New("no stream chunks configured")
	}
	ch := make(chan *gateway.ChatChunk, len(chunks)+1)
	for _, chunk := range chunks {
		ch <- &gateway.ChatChunk{
			Choices: []gateway.ChatChunkChoice{
				{Delta: gateway.ChatDelta{Content: chunk}},
			},
		}
	}
	// Final usage-bearing chunk (OGA-420), emitted when configured + requested.
	if f.streamUsage != nil && req != nil && req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		ch <- &gateway.ChatChunk{Usage: f.streamUsage}
	}
	close(ch)
	return ch, nil
}

func (f *fakeGateway) InvokeAgentStream(_ context.Context, agentName string, _ any) (<-chan *json.RawMessage, error) {
	f.delegateCalls = append(f.delegateCalls, agentName)
	if f.delegateErr != nil {
		return nil, f.delegateErr
	}
	if f.delegateRaw == nil {
		return nil, errors.New("InvokeAgentStream not configured in this test")
	}
	ch := make(chan *json.RawMessage, len(f.delegateRaw))
	for _, s := range f.delegateRaw {
		raw := json.RawMessage(s)
		ch <- &raw
	}
	close(ch)
	return ch, nil
}

// scriptedPlanner is a Planner test double (OGA-419). It yields the configured
// steps one per turn (indexed by len(PlanState.History) — the loop appends one
// observation per turn, including skips), then signals Done. The narrative is
// attached to the first turn only. When err is set it is returned on errOnTurn.
type scriptedPlanner struct {
	steps     []ToolPlanStep
	narrative string
	err       error
	errOnTurn int
}

func (p *scriptedPlanner) Next(_ context.Context, st *PlanState) (*Decision, error) {
	turn := len(st.History)
	if p.err != nil && turn == p.errOnTurn {
		return nil, p.err
	}
	if turn >= len(p.steps) {
		return &Decision{Done: true}, nil
	}
	narr := ""
	if turn == 0 {
		narr = p.narrative
	}
	step := p.steps[turn]
	return &Decision{Narrative: narr, Step: &step}, nil
}

// runPipelineForTest runs Pipeline.runInternal with a fake PlatformAccess and a
// scripted Planner, collecting all emitted events.
func runPipelineForTest(t *testing.T, gw PlatformAccess, planner Planner, input Input) []*agent.StreamEvent {
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
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "search", ToolName: "kg_search", DependsOn: -1},
		},
		narrative: "Planning...",
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
		agent.EventTypeUsage,    // per-request aggregate (OGA-420)
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
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "first", ToolName: "kg_must_succeed", DependsOn: -1, Required: true},
			{Name: "second", ToolName: "kg_after", DependsOn: -1},
		},
		narrative: "Planning...",
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
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "optional", ToolName: "kg_optional", DependsOn: -1, Required: false},
			{Name: "ok", ToolName: "kg_after", DependsOn: -1},
		},
		narrative: "Planning...",
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
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "skipped", ToolName: "kg_skipped", DependsOn: -1, Condition: "false"},
		},
		narrative: "Planning...",
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
	planner := &scriptedPlanner{narrative: "No tools..."}

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

// TestPipeline_PlannerError_FallsBackToPlainAnswer verifies the OGA-368
// behavior: a non-fatal planner failure (e.g. the LLM returned prose instead
// of JSON) degrades to a plain LLM answer instead of failing the task. The
// pipeline must emit a reasoning event explaining the degraded path, stream
// the artifact, and complete — never emit task/status{failed}.
func TestPipeline_PlannerError_FallsBackToPlainAnswer(t *testing.T) {
	gw := &fakeGateway{streamChunks: []string{"Here is", " a briefing."}}
	planner := &scriptedPlanner{err: errors.New("parse plan JSON: invalid character 'I' looking for beginning of value")}

	events := runPipelineForTest(t, gw, planner, Input{Query: "investigate this proposal"})

	hasFailed := false
	hasCompleted := false
	hasReasoning := false
	hasArtifact := false
	hasPlan := false
	for _, evt := range events {
		switch evt.Type {
		case agent.EventTypeStatus:
			if p, ok := evt.Payload.(*agent.StatusPayload); ok {
				if p.State == agent.TaskStateFailed {
					hasFailed = true
				}
				if p.State == agent.TaskStateCompleted {
					hasCompleted = true
				}
			}
		case agent.EventTypeReasoning:
			hasReasoning = true
		case agent.EventTypeArtifact:
			hasArtifact = true
		case agent.EventTypePlan:
			hasPlan = true
		}
	}

	if hasFailed {
		t.Errorf("planner parse failure must NOT emit task/status{failed}; expected graceful fallback")
	}
	if !hasCompleted {
		t.Errorf("expected task/status{completed} after fallback to plain answer")
	}
	if !hasReasoning {
		t.Errorf("expected a task/reasoning event explaining the degraded path")
	}
	if !hasArtifact {
		t.Errorf("expected the plain answer streamed as task/artifact")
	}
	if hasPlan {
		t.Errorf("expected NO plan event on the planner-failure fallback path")
	}
}

// TestPipeline_PlannerError_AssemblyAlsoFails_EmitsFailed verifies that the
// fallback does not mask a genuine transport failure: when planning fails AND
// the subsequent plain-answer assembly call also fails (gateway down), the
// pipeline surfaces task/status{failed} (per OGA-368 acceptance criteria).
func TestPipeline_PlannerError_AssemblyAlsoFails_EmitsFailed(t *testing.T) {
	gw := &fakeGateway{
		// No stream chunks → ChatCompletionStream errors → falls through to
		// non-streaming ChatCompletion, which also errors.
		completionErr: errors.New("gateway unreachable"),
	}
	planner := &scriptedPlanner{err: errors.New("parse plan JSON: invalid character 'I' looking for beginning of value")}

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
		t.Errorf("expected task/status{failed} when the fallback assembly call also fails (transport down)")
	}
}

// TestPipeline_PlannerError_ContextCancelled_EmitsFailed verifies that a
// planner failure under a cancelled context is terminal — the pipeline does
// NOT attempt a fallback and surfaces task/status{failed} (per OGA-368).
func TestPipeline_PlannerError_ContextCancelled_EmitsFailed(t *testing.T) {
	gw := &fakeGateway{streamChunks: []string{"should not be used"}}
	planner := &scriptedPlanner{err: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running so ctx.Err() != nil

	pipeline := NewPipeline()
	events := make(chan *agent.StreamEvent, 64)
	collected := make([]*agent.StreamEvent, 0, 8)
	done := make(chan struct{})
	go func() {
		for evt := range events {
			collected = append(collected, evt)
		}
		close(done)
	}()
	_ = pipeline.runInternal(ctx, Deps{Config: DefaultConfig()}, Input{Query: "q"}, planner, events, gw)
	close(events)
	<-done

	hasFailed := false
	calledGateway := len(gw.callToolCalls) > 0 || gw.completionCalls > 0
	for _, evt := range collected {
		if evt.Type == agent.EventTypeStatus {
			if p, ok := evt.Payload.(*agent.StatusPayload); ok && p.State == agent.TaskStateFailed {
				hasFailed = true
			}
		}
	}
	if !hasFailed {
		t.Errorf("expected task/status{failed} when planning fails under a cancelled context")
	}
	if calledGateway {
		t.Errorf("expected NO fallback gateway call under a cancelled context")
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

// TestPipeline_DependentStep_ChipExcludesPriorResult is the OGA-314 regression
// guard. When a step has DependsOn >= 0, the executor injects _prior_result
// into the args map for handlers that re-parse it. The chip emission MUST NOT
// expose this internal injection — it bloats the operator UI and Go's
// alphabetical map-key sort makes it the first visible argument, hiding the
// LLM-planned arguments behind the chip's truncation.
//
// Two invariants checked:
//  1. The emitted task/tool_call payload's Arguments map does NOT contain
//     "_prior_result" for either step.
//  2. The dependent step's gateway call (params received by CallTool) DOES
//     contain "_prior_result" so handlers that fall back to parsing it
//     (e.g. relationship handler's extractEntityIDFromPriorResult) keep working.
func TestPipeline_DependentStep_ChipExcludesPriorResult(t *testing.T) {
	gw := &fakeGateway{
		tools: map[string]json.RawMessage{
			"kg_search": json.RawMessage(`{"results":[{"entity_id":"019e38e3-69b7-7d81-8e09-f28b947b98a6","entity_type":"brick_Chiller"}]}`),
			"kg_reason": json.RawMessage(`{"results":[],"total_count":0}`),
		},
		streamChunks: []string{"Done."},
	}
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "search", ToolName: "kg_search", DependsOn: -1, Arguments: map[string]any{"query": "chiller"}},
			{Name: "reason", ToolName: "kg_reason", DependsOn: 0, Arguments: map[string]any{
				"mode":            "root_cause",
				"start_entity_id": "<from step 0>",
				"stop_conditions": map[string]any{"max_depth": 5},
			}},
		},
		narrative: "Planning...",
	}

	events := runPipelineForTest(t, gw, planner, Input{Query: "what caused the chiller fault?"})

	// Invariant 1: no emitted tool_call carries _prior_result.
	for _, evt := range events {
		if evt.Type != agent.EventTypeToolCall {
			continue
		}
		p, ok := evt.Payload.(*agent.ToolCallPayload)
		if !ok {
			continue
		}
		if _, has := p.Arguments["_prior_result"]; has {
			t.Errorf("tool_call event for %q exposes _prior_result in Arguments — operator chip should not see internal injection", p.ToolName)
		}
	}

	// Invariant 2: the dependent step's gateway call received _prior_result.
	if len(gw.callToolCalls) < 2 {
		t.Fatalf("expected 2 gateway calls (kg_search + kg_reason), got %d", len(gw.callToolCalls))
	}
	reasonCall := gw.callToolCalls[1]
	if reasonCall.Tool != "kg_reason" {
		t.Fatalf("expected second call to kg_reason, got %s", reasonCall.Tool)
	}
	params, ok := reasonCall.Params.(map[string]any)
	if !ok {
		t.Fatalf("expected kg_reason params to be map[string]any, got %T", reasonCall.Params)
	}
	if _, has := params["_prior_result"]; !has {
		t.Error("gateway-bound kg_reason args missing _prior_result — handlers that re-parse it for fallback ID resolution will break")
	}
	if params["mode"] != "root_cause" {
		t.Errorf("kg_reason params.mode = %v, want root_cause", params["mode"])
	}
	// start_entity_id should be resolved from the upstream entity_id.
	if got := params["start_entity_id"]; got != "019e38e3-69b7-7d81-8e09-f28b947b98a6" {
		t.Errorf("kg_reason params.start_entity_id = %v, want resolved UUID", got)
	}
}

// TestPipeline_ExecutorDoesNotMutatePlanArguments verifies that running the
// pipeline does not retain the _prior_result mutation on the original plan's
// step.Arguments map. Without the cloneArgs() guard in executeStep, re-runs
// of the same plan would carry forward the previous run's _prior_result.
func TestPipeline_ExecutorDoesNotMutatePlanArguments(t *testing.T) {
	gw := &fakeGateway{
		tools: map[string]json.RawMessage{
			"kg_search": json.RawMessage(`{"results":[{"entity_id":"e1"}]}`),
			"kg_reason": json.RawMessage(`{"results":[]}`),
		},
		streamChunks: []string{"ok"},
	}
	originalArgs := map[string]any{
		"mode":            "impact_chain",
		"start_entity_id": "<from step 0>",
	}
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "search", ToolName: "kg_search", DependsOn: -1},
			{Name: "reason", ToolName: "kg_reason", DependsOn: 0, Arguments: originalArgs},
		},
		narrative: "Planning...",
	}

	_ = runPipelineForTest(t, gw, planner, Input{Query: "q"})

	if _, has := originalArgs["_prior_result"]; has {
		t.Error("executor mutated the plan's step.Arguments map — _prior_result leaked into the original. Re-runs of the same plan will carry stale upstream data.")
	}
	if got := originalArgs["start_entity_id"]; got != "<from step 0>" {
		t.Errorf("executor mutated start_entity_id placeholder in original args (now %v) — clone was not effective", got)
	}
}
