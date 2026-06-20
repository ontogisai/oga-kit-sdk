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
// adapted to the ReAct loop in OGA-419). It guarantees grounding on the
// proposal's concrete seed entities, then delegates to an inner LLM planner for
// question-relevant evidence (SOP, history, trends) — the same dynamic planner
// the reactive chat surface uses.
//
// Per turn:
//
//  1. While not every seed entity has been fetched, it emits ONE deterministic
//     kg_get_entity for the next seed (one per turn). This anchors the briefing
//     on the proposal's actual subject even if the LLM later gathers nothing.
//  2. Once all seeds are fetched, it delegates to the inner LLM planner (which
//     sees the seed observations in PlanState.History and plans complementary
//     evidence retrieval over the full toolbox).
//
// It is stateless: the number of seeds already fetched is derived from
// len(PlanState.History), since the loop calls Next sequentially and the seed
// steps are issued first.
type InvestigationLLMPlanner struct {
	entityIDs []string
	inner     Planner
}

// NewInvestigationLLMPlanner constructs the planner for the given seed entity
// ids and an inner planner (typically the profile's reactive LLMToolPlanner).
// Ids are deduped, blank-stripped, and capped at maxInvestigationEntities.
func NewInvestigationLLMPlanner(entityIDs []string, inner Planner) *InvestigationLLMPlanner {
	return &InvestigationLLMPlanner{entityIDs: normalizeEntityIDs(entityIDs), inner: inner}
}

// Next front-loads the seed kg_get_entity steps, then delegates to the inner
// planner with the query augmented to tell the LLM the seeds are already being
// retrieved (so it plans ADDITIONAL evidence rather than re-listing them).
func (p *InvestigationLLMPlanner) Next(ctx context.Context, st *PlanState) (*Decision, error) {
	fetched := len(st.History)

	if fetched < len(p.entityIDs) {
		id := p.entityIDs[fetched]
		narrative := ""
		if fetched == 0 {
			narrative = investigationNarrative(p.entityIDs).Text
		}
		return &Decision{
			Narrative: narrative,
			Step: &ToolPlanStep{
				Name:      fmt.Sprintf("seed_entity_%d", fetched),
				ToolName:  "kg_get_entity",
				Arguments: map[string]any{"entity_id": id},
				DependsOn: -1,
				Rationale: fmt.Sprintf("Ground the briefing on the proposal's entity %s", id),
			},
		}, nil
	}

	// Seeds done. Defensive: no inner planner → finalize on seed-only grounding.
	if p.inner == nil {
		return &Decision{Done: true}, nil
	}

	// Delegate, augmenting the query so the LLM plans complementary evidence.
	augmented := *st
	augmented.Query = augmentInvestigationQuery(st.Query, p.entityIDs)
	return p.inner.Next(ctx, &augmented)
}

// augmentInvestigationQuery appends a planning directive telling the LLM that
// the seed entities are already being retrieved, so it should plan
// COMPLEMENTARY evidence (SOP, history, trends) rather than re-listing them.
func augmentInvestigationQuery(query string, ids []string) string {
	if len(ids) == 0 {
		return query
	}
	return query + fmt.Sprintf(
		"\n\n[Planning note: %s already retrieved via kg_get_entity. Gather "+
			"ADDITIONAL evidence to answer the operator and, where relevant, assess "+
			"the proposal — e.g. recent sensor readings/trends (kg_ts_read, "+
			"kg_ts_analyze), related equipment (kg_traverse, kg_query_entities), the "+
			"governing SOP (kg_doc_content), prior work orders, or similar past "+
			"incidents (kg_vector). Do not re-fetch the seed entities.]",
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
