package streampipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// LLMToolPlanner implements StreamPlanner by asking the LLM to produce a JSON
// tool-call plan dynamically. Used by the platform's Knowledge Agent and by
// kit-supplied domain agents whose profile does NOT declare a grounding
// strategy.
type LLMToolPlanner struct {
	gw      gatewayClient
	profile *agent.DomainAgentProfile
	cfg     agent.PlannerConfig
	logger  *slog.Logger
}

// NewLLMToolPlanner constructs an LLM-driven planner. The gateway client is
// used for the LLM chat completion call; the profile contributes the system
// prompt + the tool union.
func NewLLMToolPlanner(gw *gateway.PlatformGatewayClient, profile *agent.DomainAgentProfile, cfg agent.PlannerConfig) *LLMToolPlanner {
	return &LLMToolPlanner{
		gw:      gw,
		profile: profile,
		cfg:     cfg,
		logger:  slog.Default(),
	}
}

// NewLLMToolPlannerWithClient is the testable constructor that takes any
// gatewayClient implementation (for mocking in tests).
func NewLLMToolPlannerWithClient(gw gatewayClient, profile *agent.DomainAgentProfile, cfg agent.PlannerConfig) *LLMToolPlanner {
	return &LLMToolPlanner{
		gw:      gw,
		profile: profile,
		cfg:     cfg,
		logger:  slog.Default(),
	}
}

// Plan asks the LLM for a tool-call plan and converts it to the streampipeline
// ToolPlan shape. Falls back to one retry on parse failure (the LLM
// occasionally returns prose instead of JSON on the first attempt).
func (p *LLMToolPlanner) Plan(ctx context.Context, query string, tools []string) (*ToolPlan, *PlanNarrative, error) {
	// Use the union of profile tools when caller doesn't supply explicit tools.
	if len(tools) == 0 {
		tools = agent.UniqueTools(p.profile)
	}
	if len(tools) == 0 {
		// No tools available — return empty plan; the pipeline falls back to
		// plainAnswer (single LLM call, no grounding).
		return &ToolPlan{}, &PlanNarrative{Text: "No tools available; answering directly."}, nil
	}

	// RequestPlan is self-correcting (OGA-387): on a parse failure it retries
	// once with a corrective turn. No outer retry loop is needed here.
	rawPlan, err := agent.RequestPlan(ctx, p.gw, p.profile, query, tools, p.cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("LLM planning: %w", err)
	}

	return convertAgentPlan(rawPlan), &PlanNarrative{Text: "Planning which tools to use for your query..."}, nil
}

// convertAgentPlan translates the existing agent.ToolPlan (returned by
// agent.RequestPlan) into the streampipeline.ToolPlan shape. The translation
// is mechanical — Required/Condition/MaxResults stay at zero defaults because
// the LLM doesn't author those fields (they're a kit-author concern).
func convertAgentPlan(p *agent.ToolPlan) *ToolPlan {
	if p == nil {
		return &ToolPlan{}
	}
	out := &ToolPlan{Steps: make([]ToolPlanStep, 0, len(p.Steps))}
	for i, s := range p.Steps {
		name := fmt.Sprintf("step_%d", i)
		out.Steps = append(out.Steps, ToolPlanStep{
			Name:      name,
			ToolName:  s.ToolName,
			Arguments: s.Arguments,
			DependsOn: s.DependsOn,
			Rationale: s.Rationale,
			// Required, Condition, MaxResults stay at zero defaults.
		})
	}
	return out
}
