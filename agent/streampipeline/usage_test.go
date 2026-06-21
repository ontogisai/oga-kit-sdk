package streampipeline

import (
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// findUsage returns the first task/usage payload with the given role, or nil.
func findUsage(events []*agent.StreamEvent, role string) *agent.UsagePayload {
	for _, e := range events {
		if e.Type != agent.EventTypeUsage {
			continue
		}
		if p, ok := e.Payload.(*agent.UsagePayload); ok && p.Role == role {
			return p
		}
	}
	return nil
}

// TestPipeline_AssemblyUsage_AggregateEmitted verifies the streaming assembly
// usage is captured from the final usage-bearing chunk and folded into the
// per-request aggregate (OGA-420 Gap 2).
func TestPipeline_AssemblyUsage_AggregateEmitted(t *testing.T) {
	gw := &fakeGateway{
		streamChunks: []string{"Here is", " the answer."},
		streamUsage:  &gateway.Usage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
	}
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{{Name: "search", ToolName: "kg_search", DependsOn: -1}},
	}
	events := runPipelineForTest(t, gw, planner, Input{Query: "q", Actor: agent.EventActor{ID: "a"}})

	asm := findUsage(events, agent.UsageRoleAssembly)
	if asm == nil {
		t.Fatal("expected a task/usage assembly event")
	}
	if !asm.Available || asm.Usage.TotalTokens != 150 {
		t.Errorf("assembly usage = %+v (avail=%v), want {120,30,150} avail=true", asm.Usage, asm.Available)
	}

	agg := findUsage(events, agent.UsageRoleAggregate)
	if agg == nil {
		t.Fatal("expected a task/usage aggregate event")
	}
	if !agg.Available {
		t.Error("aggregate should be Available when the proxy reported usage")
	}
	if agg.TurnIndex != -1 {
		t.Errorf("aggregate TurnIndex = %d, want -1", agg.TurnIndex)
	}
	if agg.Usage.PromptTokens != 120 || agg.Usage.CompletionTokens != 30 || agg.Usage.TotalTokens != 150 {
		t.Errorf("aggregate usage = %+v, want {120,30,150}", agg.Usage)
	}
}

// TestPipeline_Usage_UnavailableStillAggregates verifies that when the proxy
// reports no usage, exactly one aggregate event is emitted labelled unavailable
// with zero counts, and no per-call usage events are emitted (no fabrication).
func TestPipeline_Usage_UnavailableStillAggregates(t *testing.T) {
	gw := &fakeGateway{streamChunks: []string{"answer"}} // no streamUsage
	planner := &scriptedPlanner{steps: []ToolPlanStep{{Name: "s", ToolName: "kg_search", DependsOn: -1}}}
	events := runPipelineForTest(t, gw, planner, Input{Query: "q", Actor: agent.EventActor{ID: "a"}})

	if a := findUsage(events, agent.UsageRoleAssembly); a != nil {
		t.Error("no assembly usage event expected when the proxy reported none")
	}
	if d := findUsage(events, agent.UsageRoleDecision); d != nil {
		t.Error("no decision usage event expected when the proxy reported none")
	}
	agg := findUsage(events, agent.UsageRoleAggregate)
	if agg == nil {
		t.Fatal("expected a task/usage aggregate event even with no usage")
	}
	if agg.Available {
		t.Error("aggregate should be labelled unavailable when proxy reported nothing")
	}
	if agg.Usage.TotalTokens != 0 {
		t.Errorf("unavailable aggregate should be zero, got %+v", agg.Usage)
	}
}

// TestPipeline_AssemblyModelSelection verifies the assembly LLM request carries
// the independently-configured model / max_tokens / temperature and requests a
// usage-bearing final chunk (OGA-420 Gap 1 + Gap 2).
func TestPipeline_AssemblyModelSelection(t *testing.T) {
	temp := 0.9
	gw := &fakeGateway{streamChunks: []string{"x"}}
	planner := &scriptedPlanner{} // Done on turn 0 → straight to assembly
	_ = runPipelineForTest(t, gw, planner, Input{
		Query:               "q",
		Actor:               agent.EventActor{ID: "a"},
		AssemblyModel:       "claude-sonnet",
		AssemblyMaxTokens:   4096,
		AssemblyTemperature: &temp,
	})

	var asmReq *gateway.ChatCompletionRequest
	for _, r := range gw.chatReqs {
		if r.Stream {
			asmReq = r
		}
	}
	if asmReq == nil {
		t.Fatal("no streaming assembly request recorded")
	}
	if asmReq.Model != "claude-sonnet" {
		t.Errorf("assembly model = %q, want claude-sonnet", asmReq.Model)
	}
	if asmReq.MaxTokens != 4096 {
		t.Errorf("assembly max_tokens = %d, want 4096", asmReq.MaxTokens)
	}
	if asmReq.Temperature == nil || *asmReq.Temperature != 0.9 {
		t.Errorf("assembly temperature = %v, want 0.9", asmReq.Temperature)
	}
	if asmReq.StreamOptions == nil || !asmReq.StreamOptions.IncludeUsage {
		t.Error("assembly stream should request stream_options.include_usage")
	}
}

// TestPipeline_AssemblyModel_DefaultWhenUnset verifies the no-regression default:
// absent assembly config collapses to the gateway default route + 2048 tokens.
func TestPipeline_AssemblyModel_DefaultWhenUnset(t *testing.T) {
	gw := &fakeGateway{streamChunks: []string{"x"}}
	planner := &scriptedPlanner{}
	_ = runPipelineForTest(t, gw, planner, Input{Query: "q", Actor: agent.EventActor{ID: "a"}})

	var asmReq *gateway.ChatCompletionRequest
	for _, r := range gw.chatReqs {
		if r.Stream {
			asmReq = r
		}
	}
	if asmReq == nil {
		t.Fatal("no streaming assembly request recorded")
	}
	if asmReq.Model != "" {
		t.Errorf("assembly model = %q, want empty (gateway default)", asmReq.Model)
	}
	if asmReq.MaxTokens != 2048 {
		t.Errorf("assembly max_tokens = %d, want 2048 default", asmReq.MaxTokens)
	}
	if asmReq.Temperature != nil {
		t.Errorf("assembly temperature = %v, want nil (gateway default)", asmReq.Temperature)
	}
}
