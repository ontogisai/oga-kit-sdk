package streampipeline

import (
	"context"
	"fmt"
)

// maxInvestigationEntities caps how many seed entities a reactive investigation
// grounds on, bounding the seed-step count when a convergence proposal
// correlates many entities. The typical proactive case has exactly one.
const maxInvestigationEntities = 5

// InvestigationLLMPlanner is the reactive [Investigate] planner (OGA-378,
// reworked per Option 2). It combines guaranteed grounding with the agent's
// FULL tool union, replacing the original fixed two-tool plan
// (kg_get_entity + kg_traverse) that could not reach the SOP / history / trend
// evidence the proposal was based on — which forced the briefing to speculate.
//
// Per request it:
//
//  1. ALWAYS front-loads a kg_get_entity per concrete seed entity
//     (InvestigationContext target/trigger ids) so the briefing is anchored to
//     the proposal's actual subject — deterministic, never skipped, even if
//     the LLM plan is empty or the planning call fails.
//  2. Delegates to the agent's LLM tool planner (the SAME planner the reactive
//     chat surface uses, carrying the full profile tool union) to add
//     question-relevant evidence retrieval — the governing SOP via
//     kg_doc_content, recent trends via kg_ts_read / kg_ts_analyze, prior work
//     orders via kg_query_entities, related equipment via kg_traverse, etc. The
//     LLM picks whatever the operator's question and the proposal demand.
//
// The seed steps guarantee grounding; the LLM steps give the briefing the same
// evidence base the proactive path had — without replaying the proactive
// grounding_strategy's event-only placeholders (the OGA-348 hazard).
type InvestigationLLMPlanner struct {
	entityIDs []string
	llm       StreamPlanner
}

// NewInvestigationLLMPlanner constructs the planner for the given seed entity
// ids and an inner LLM planner (typically the profile's reactive
// LLMToolPlanner, which carries the full tool union). Callers pass the
// InvestigationContext seed ids; the planner dedupes, drops blanks, and caps at
// maxInvestigationEntities.
func NewInvestigationLLMPlanner(entityIDs []string, llm StreamPlanner) *InvestigationLLMPlanner {
	return &InvestigationLLMPlanner{entityIDs: normalizeEntityIDs(entityIDs), llm: llm}
}

// Plan builds a seed kg_get_entity step for each concrete entity, then appends
// the inner LLM planner's complementary steps with their DependsOn indices
// offset past the seed steps (DependsOn is an index into the executed-steps
// slice; prepending N seed steps shifts every dependent LLM step by N). The LLM
// is told the seed entities are already being fetched so it plans ADDITIONAL
// evidence rather than re-listing them.
//
// The plan never fails on an LLM hiccup: an empty/errored inner plan degrades
// to seed-only grounding so the briefing still anchors on the entities.
func (p *InvestigationLLMPlanner) Plan(ctx context.Context, query string, tools []string) (*ToolPlan, *PlanNarrative, error) {
	seed := make([]ToolPlanStep, 0, len(p.entityIDs))
	for i, id := range p.entityIDs {
		seed = append(seed, ToolPlanStep{
			Name:      fmt.Sprintf("seed_entity_%d", i),
			ToolName:  "kg_get_entity",
			Arguments: map[string]any{"entity_id": id},
			DependsOn: -1,
			Rationale: fmt.Sprintf("Ground the briefing on the proposal's entity %s", id),
			Required:  false,
		})
	}

	// No inner planner (defensive) → seed-only grounding.
	if p.llm == nil {
		return &ToolPlan{Steps: seed}, investigationNarrative(p.entityIDs), nil
	}

	// Ask the LLM to plan complementary evidence retrieval over the full toolbox.
	llmPlan, _, err := p.llm.Plan(ctx, augmentInvestigationQuery(query, p.entityIDs), tools)
	if err != nil || llmPlan == nil || len(llmPlan.Steps) == 0 {
		// Degrade to seed-only grounding — never fail the investigation on an
		// LLM planning hiccup (the briefing still grounds on the entities).
		return &ToolPlan{Steps: seed}, investigationNarrative(p.entityIDs), nil
	}

	offset := len(seed)
	merged := make([]ToolPlanStep, 0, offset+len(llmPlan.Steps))
	merged = append(merged, seed...)
	for _, s := range llmPlan.Steps {
		if s.DependsOn >= 0 {
			s.DependsOn += offset
		}
		merged = append(merged, s)
	}
	return &ToolPlan{Steps: merged}, investigationNarrative(p.entityIDs), nil
}

// augmentInvestigationQuery appends a planning directive telling the LLM that
// the seed entities are already being retrieved, so it should plan
// COMPLEMENTARY evidence (SOP, history, trends) rather than re-listing them.
func augmentInvestigationQuery(query string, ids []string) string {
	if len(ids) == 0 {
		return query
	}
	return query + fmt.Sprintf(
		"\n\n[Planning note: %s already being retrieved via kg_get_entity. Plan "+
			"ADDITIONAL tools to answer the operator's question and, where relevant, "+
			"assess the proposal — e.g. recent sensor readings/trends (kg_ts_read, "+
			"kg_ts_analyze), related equipment (kg_traverse, kg_query_entities), the "+
			"governing SOP (kg_doc_content), prior work orders, or similar past "+
			"incidents (kg_vector). Do not re-list the seed entities.]",
		describeEntities(ids))
}

func investigationNarrative(ids []string) *PlanNarrative {
	if len(ids) == 0 {
		return &PlanNarrative{Text: "Investigating the proposal — answering from the available context..."}
	}
	return &PlanNarrative{
		Text: fmt.Sprintf("Investigating the proposal — gathering live data on %s plus supporting evidence from the knowledge graph...",
			describeEntities(ids)),
	}
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
