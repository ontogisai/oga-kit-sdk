package streampipeline

import (
	"context"
	"fmt"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// GroundingStrategyPlanner implements StreamPlanner deterministically — no
// LLM call. Reads the kit-declared grounding_strategy from the agent profile
// and converts each step to a ToolPlanStep, preserving Required, Condition,
// MaxResults, and DependsOn semantics.
//
// Used by kit-supplied domain agents whose profile YAML declares
// `proactive_reasoning.grounding_strategy:`. When the profile declares no
// strategy, DefaultRuntime falls back to LLMToolPlanner instead.
type GroundingStrategyPlanner struct {
	profile *agent.DomainAgentProfile
}

// NewGroundingStrategyPlanner constructs a planner that uses the kit's
// declared grounding strategy. If profile is nil or has no grounding strategy,
// Plan returns an empty ToolPlan (the pipeline will fall back to plain LLM
// answer with no grounding).
func NewGroundingStrategyPlanner(profile *agent.DomainAgentProfile) *GroundingStrategyPlanner {
	return &GroundingStrategyPlanner{profile: profile}
}

// Plan converts the kit-declared GroundingStrategy slice into a ToolPlan
// with named-placeholder substitution preserved. The narrative reflects the
// agent's persona (e.g., "Investigating with FM Operations Agent persona...").
func (p *GroundingStrategyPlanner) Plan(_ context.Context, _ string, _ []string) (*ToolPlan, *PlanNarrative, error) {
	if p.profile == nil || p.profile.ProactiveReasoning == nil || len(p.profile.ProactiveReasoning.GroundingStrategy) == 0 {
		return &ToolPlan{}, &PlanNarrative{Text: "No grounding strategy declared; answering directly."}, nil
	}

	strategy := p.profile.ProactiveReasoning.GroundingStrategy

	// First pass: build name → index map for DependsOn resolution.
	nameToIndex := make(map[string]int, len(strategy))
	for i, step := range strategy {
		if step.Name != "" {
			nameToIndex[step.Name] = i
		}
	}

	// Second pass: convert to ToolPlanStep slice.
	steps := make([]ToolPlanStep, 0, len(strategy))
	for i, gs := range strategy {
		dependsOn := -1
		if gs.DependsOn != "" {
			if idx, ok := nameToIndex[gs.DependsOn]; ok && idx < i {
				dependsOn = idx
			}
			// If the depends_on name doesn't resolve to a prior step, leave
			// at -1 (executor will treat as independent). Logging this is
			// the kit-validation tool's job, not the planner's.
		}

		steps = append(steps, ToolPlanStep{
			Name:       gs.Name,
			ToolName:   gs.Tool,
			Arguments:  gs.Arguments,
			DependsOn:  dependsOn,
			Rationale:  fmt.Sprintf("Grounding step %q via %s", gs.Name, gs.Tool),
			Required:   gs.Required,
			Condition:  gs.Condition,
			MaxResults: gs.MaxResults,
		})
	}

	personaName := "agent"
	if p.profile.Name != "" {
		personaName = p.profile.Name
	}
	narrative := &PlanNarrative{
		Text: fmt.Sprintf("Investigating with %s persona...", personaName),
	}

	return &ToolPlan{Steps: steps}, narrative, nil
}
