package streampipeline

import (
	"context"
	"fmt"
)

// maxInvestigationEntities caps how many seed entities a reactive investigation
// grounds on, bounding the plan size when a convergence proposal correlates
// many entities. The typical proactive case has exactly one.
const maxInvestigationEntities = 5

// InvestigationGroundingPlanner is the reactive-safe, deterministic planner for
// the [Investigate] follow-up (OGA-378). Unlike GroundingStrategyPlanner (which
// reads the kit's proactive grounding_strategy and resolves {entity_id}
// placeholders that only exist on the proactive path), this planner is seeded
// by CONCRETE entity ids carried on the investigation forward
// (InvestigationContext.trigger_entity_ids). It emits a fixed, domain-agnostic
// retrieval plan per entity so the agent's briefing is grounded in real KG data
// rather than a plain LLM completion.
//
// Per seed entity (capped at maxInvestigationEntities) it plans:
//   - kg_get_entity{entity_id}      — the entity's real properties (the core
//     grounding; valid for every entity type)
//   - kg_traverse{start_entity_id}  — 1-hop neighbors (related equipment),
//     all relationship types, outgoing
//
// Both steps are non-Required: a stale/missing id or an empty traversal
// degrades gracefully (logged, skipped) and the assembly still grounds on
// whatever resolved. When no entity resolves, the pipeline's plain-answer
// fallback (OGA-368) still produces a useful response.
type InvestigationGroundingPlanner struct {
	entityIDs []string
}

// NewInvestigationGroundingPlanner constructs the planner for the given seed
// entity ids. Callers pass InvestigationContext.trigger_entity_ids; the planner
// dedupes, drops blanks, and caps the list.
func NewInvestigationGroundingPlanner(entityIDs []string) *InvestigationGroundingPlanner {
	return &InvestigationGroundingPlanner{entityIDs: normalizeEntityIDs(entityIDs)}
}

// Plan builds the deterministic per-entity retrieval plan. The query is unused
// (the seed entities, not the prose, drive grounding); tools are unused (the
// plan references fixed platform Tier-1 tools).
func (p *InvestigationGroundingPlanner) Plan(_ context.Context, _ string, _ []string) (*ToolPlan, *PlanNarrative, error) {
	if len(p.entityIDs) == 0 {
		// Nothing to ground on — empty plan; the pipeline falls back to a
		// plain answer (no hard failure).
		return &ToolPlan{}, &PlanNarrative{Text: "No investigation entities supplied; answering directly."}, nil
	}

	steps := make([]ToolPlanStep, 0, len(p.entityIDs)*2)
	for i, id := range p.entityIDs {
		steps = append(steps, ToolPlanStep{
			Name:      fmt.Sprintf("entity_%d", i),
			ToolName:  "kg_get_entity",
			Arguments: map[string]any{"entity_id": id},
			DependsOn: -1,
			Rationale: fmt.Sprintf("Retrieve the triggering entity %s and its properties", id),
			Required:  false,
		})
		steps = append(steps, ToolPlanStep{
			Name:     fmt.Sprintf("neighbors_%d", i),
			ToolName: "kg_traverse",
			Arguments: map[string]any{
				"start_entity_id": id,
				"direction":       "outgoing",
				"max_depth":       1,
			},
			DependsOn: -1,
			Rationale: fmt.Sprintf("Find equipment and locations related to %s (1 hop)", id),
			Required:  false,
		})
	}

	narrative := &PlanNarrative{
		Text: fmt.Sprintf("Investigating the proposal — gathering live data on %s from the knowledge graph...",
			describeEntities(p.entityIDs)),
	}
	return &ToolPlan{Steps: steps}, narrative, nil
}

// normalizeEntityIDs trims blanks, dedupes (preserving first-seen order), and
// caps at maxInvestigationEntities.
func normalizeEntityIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) == maxInvestigationEntities {
			break
		}
	}
	return out
}

// describeEntities renders a short human phrase for the narrative.
func describeEntities(ids []string) string {
	switch len(ids) {
	case 0:
		return "the proposal"
	case 1:
		return "the triggering entity"
	default:
		return fmt.Sprintf("%d correlated entities", len(ids))
	}
}
