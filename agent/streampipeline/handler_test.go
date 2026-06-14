package streampipeline

import (
	"context"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// newTestRuntime builds a minimal DefaultRuntime for planner-selection tests.
// The gateway client is constructed but never dialed — reactiveStreamPlanner
// only needs it to hand to NewLLMToolPlanner; it makes no network call.
func newTestRuntime(t *testing.T, profile *agent.DomainAgentProfile) *agent.DefaultRuntime {
	t.Helper()
	deps, err := agent.ConnectRuntimeDeps(context.Background(), &agent.RuntimeDepsConfig{
		GatewayURL: "http://localhost:0",
		TenantID:   "test-tenant",
		AgentID:    "test-agent",
	})
	if err != nil {
		t.Fatalf("ConnectRuntimeDeps: %v", err)
	}
	return agent.NewDefaultRuntime(profile, deps)
}

// TestReactiveStreamPlanner_AlwaysLLM is the OGA-348 regression guard: the
// reactive streaming path must use LLM-driven planning (like the Knowledge
// Agent) regardless of whether the profile declares a proactive grounding
// strategy. Running the grounding strategy on the reactive path replays a
// rigid plan with unsubstituted event placeholders (e.g. {entity_id}) and
// breaks both interactive chat and the [Investigate] follow-up.
func TestReactiveStreamPlanner_AlwaysLLM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile *agent.DomainAgentProfile
	}{
		{
			name: "no grounding strategy",
			profile: &agent.DomainAgentProfile{
				Name:    "Knowledge-style Agent",
				AgentID: "ka-style",
			},
		},
		{
			name: "with grounding strategy (proactive-only)",
			profile: &agent.DomainAgentProfile{
				Name:    "FM Operations Agent",
				AgentID: "fm-operations-agent",
				ProactiveReasoning: &agent.ProactiveConfig{
					GroundingStrategy: []agent.GroundingStep{
						{Name: "trigger_entity", Tool: "kg_get_entity", Required: true,
							Arguments: map[string]any{"entity_id": "{entity_id}"}},
						{Name: "history", Tool: "kg_query_entities",
							Arguments: map[string]any{"entity_type": "WorkOrder"}},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := newTestRuntime(t, tt.profile)
			planner := reactiveStreamPlanner(rt)
			if _, ok := planner.(*LLMToolPlanner); !ok {
				t.Fatalf("reactive planner = %T, want *LLMToolPlanner — the grounding strategy must stay proactive-only (OGA-348)", planner)
			}
			if _, ok := planner.(*GroundingStrategyPlanner); ok {
				t.Fatalf("reactive planner must NOT be a GroundingStrategyPlanner (OGA-348)")
			}
		})
	}
}
