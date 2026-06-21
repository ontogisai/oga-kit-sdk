package streampipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// LLMToolPlanner implements Planner by asking the LLM for the SINGLE next action
// each turn, given the full observation transcript (OGA-419). It is the core
// ReAct planner used by the platform's Knowledge Agent, by domain agents'
// reactive chat / Investigate paths, and (seeded with grounding hints) by the
// proactive proposal path.
//
// It is stateless across turns: everything it needs — persona + tool palette,
// grounding hints, seed facts, observation history — arrives in PlanState. A
// single instance is therefore safe to reuse and the loop stays fully
// transcript-driven.
type LLMToolPlanner struct {
	gw     PlatformAccess
	cfg    agent.PlannerConfig
	logger *slog.Logger
}

// NewLLMToolPlanner constructs the ReAct LLM planner. The persona + tool palette
// are supplied per turn via PlanState.Persona, so the KA (no DomainAgentProfile)
// and domain agents share one constructor.
func NewLLMToolPlanner(gw PlatformAccess, cfg agent.PlannerConfig) *LLMToolPlanner {
	return &LLMToolPlanner{gw: gw, cfg: cfg, logger: slog.Default()}
}

// Next asks the LLM for the next action against the observations so far and maps
// the decision into the pipeline's Decision shape. A "final" decision ends the
// loop; otherwise it returns the chosen tool + arguments as a ToolPlanStep
// (DependsOn -1 — the model emits concrete arguments because it has already
// observed the prior results).
func (p *LLMToolPlanner) Next(ctx context.Context, st *PlanState) (*Decision, error) {
	req := agent.NextStepRequest{
		SystemPrompt: st.Persona.SystemPrompt,
		Tools:        st.Persona.Tools,
		ToolSchemas:  st.Persona.ToolSchemas,
		Query:        st.Query,
		SeedFacts:    st.SeedFacts,
		Hints:        groundingHints(st.GroundingStrategy),
		History:      toObservations(st.History),
	}

	d, err := agent.RequestNextStep(ctx, p.gw, req, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("LLM next-step: %w", err)
	}

	if d.Final {
		return &Decision{Done: true, Narrative: d.Thought, Usage: d.Usage, UsageAvailable: d.UsageAvailable}, nil
	}

	return &Decision{
		Narrative: d.Thought,
		Step: &ToolPlanStep{
			Name:      fmt.Sprintf("step_%d", len(st.History)),
			ToolName:  d.ToolName,
			Arguments: d.Arguments,
			DependsOn: -1,
		},
		Usage:          d.Usage,
		UsageAvailable: d.UsageAvailable,
	}, nil
}

// groundingHints converts kit-declared GroundingStep entries into the
// planner-facing advisory hint shape. Arguments are intentionally NOT carried:
// they may contain proactive {placeholder} tokens, and under ReAct the model
// derives concrete arguments from SeedFacts + observations. The tool name,
// rationale, and strongly-advised flag are the useful guidance (OGA-419).
func groundingHints(steps []agent.GroundingStep) []agent.GroundingHint {
	if len(steps) == 0 {
		return nil
	}
	out := make([]agent.GroundingHint, 0, len(steps))
	for _, s := range steps {
		out = append(out, agent.GroundingHint{
			Tool:            s.Tool,
			Rationale:       s.Name,
			StronglyAdvised: s.Required,
		})
	}
	return out
}

// toObservations maps the executed-step transcript into the planner's
// observation shape, truncating each result so the prompt stays within budget.
func toObservations(results []ToolStepResult) []agent.NextStepObservation {
	if len(results) == 0 {
		return nil
	}
	const maxObsBytes = 2048
	out := make([]agent.NextStepObservation, 0, len(results))
	for _, r := range results {
		content := r.Content
		if len(content) > maxObsBytes {
			content = content[:maxObsBytes] + "…(truncated)"
		}
		errText := r.Error
		if r.Skipped && errText == "" {
			errText = r.SkipReason
		}
		out = append(out, agent.NextStepObservation{
			ToolName: r.ToolName,
			Success:  r.Success,
			Content:  content,
			Error:    errText,
			Skipped:  r.Skipped,
		})
	}
	return out
}
