package streampipeline

import (
	"context"
	"fmt"
)

// maxInvestigationEntities caps how many seed entities a reactive investigation
// grounds on, bounding the seed payload when a convergence proposal correlates
// many entities. The typical proactive case has exactly one. The cap also keeps
// the batched fetch (one kg_get_entity with entity_ids) well under the MCP
// tool's 100-id batch ceiling.
const maxInvestigationEntities = 5

// InvestigationLLMPlanner is the reactive [Investigate] planner (OGA-378,
// adapted to the ReAct loop in OGA-419). It guarantees grounding on the
// proposal's concrete seed entities, then delegates to an inner LLM planner for
// question-relevant evidence (SOP, history, trends) — the same dynamic planner
// the reactive chat surface uses.
//
// Seed retrieval (OGA-419 follow-up optimization):
//
//  1. Batched (default): on the FIRST turn it emits ONE deterministic
//     kg_get_entity call carrying ALL seed ids in entity_ids (the MCP tool
//     supports batch retrieval). One turn instead of N sequential turns — the
//     win for convergence proposals that correlate several entities. This
//     anchors the briefing on the proposal's actual subjects with fresh state.
//  2. Skipped (seedsAlreadyGrounded): when the caller signals that THIS
//     conversation thread already grounded these seeds recently (within the
//     freshness window — decided upstream by Frontier from per-agent history),
//     the seed fetch is skipped entirely. The prior turn's entity snapshot is
//     already in the injected per-agent history, so the planner delegates
//     straight to the inner LLM planner with a note telling it the seeds are
//     grounded and to re-fetch only if it needs CURRENT live values.
//
// Once the seed step is done (or skipped), it delegates to the inner LLM
// planner, which sees the seed observation in PlanState.History and plans
// complementary evidence retrieval over the full toolbox.
//
// It is stateless: "has the seed batch run yet" is derived from
// len(PlanState.History) == 0, since the loop calls Next sequentially and the
// seed step is always issued first.
type InvestigationLLMPlanner struct {
	entityIDs []string
	inner     Planner
	// seedsAlreadyGrounded skips the deterministic seed fetch because the
	// conversation thread already grounded these entities recently (OGA-419).
	seedsAlreadyGrounded bool
}

// InvestigationOption configures an InvestigationLLMPlanner.
type InvestigationOption func(*InvestigationLLMPlanner)

// WithSeedsAlreadyGrounded marks the seed entities as already grounded earlier
// in this conversation thread (within the freshness window), so the planner
// skips the deterministic seed fetch and relies on the injected per-agent
// history. Set by the handler from the inbound investigation_seeds_grounded
// metadata flag (Frontier decides freshness from per-agent history recency).
func WithSeedsAlreadyGrounded() InvestigationOption {
	return func(p *InvestigationLLMPlanner) { p.seedsAlreadyGrounded = true }
}

// NewInvestigationLLMPlanner constructs the planner for the given seed entity
// ids and an inner planner (typically the profile's reactive LLMToolPlanner).
// Ids are deduped, blank-stripped, and capped at maxInvestigationEntities.
func NewInvestigationLLMPlanner(entityIDs []string, inner Planner, opts ...InvestigationOption) *InvestigationLLMPlanner {
	p := &InvestigationLLMPlanner{entityIDs: normalizeEntityIDs(entityIDs), inner: inner}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Next emits the (batched) seed kg_get_entity step, then delegates to the inner
// planner with the query augmented to tell the LLM the seeds are already being
// retrieved (so it plans ADDITIONAL evidence rather than re-listing them). When
// seedsAlreadyGrounded is set it skips the seed fetch entirely and delegates
// immediately with a "grounded earlier in this thread" note.
func (p *InvestigationLLMPlanner) Next(ctx context.Context, st *PlanState) (*Decision, error) {
	// Option 1 (OGA-419): the thread already grounded these seeds recently —
	// skip the fetch and reason over the injected per-agent history.
	if p.seedsAlreadyGrounded {
		if p.inner == nil {
			return &Decision{Done: true}, nil
		}
		augmented := *st
		augmented.Query = augmentInvestigationQueryGrounded(st.Query, p.entityIDs)
		return p.inner.Next(ctx, &augmented)
	}

	// Option 2 (OGA-419): batch the seed fetch — one kg_get_entity for ALL
	// seeds on the first turn (empty history), instead of one per turn.
	if len(p.entityIDs) > 0 && len(st.History) == 0 {
		return &Decision{
			Narrative: investigationNarrative(p.entityIDs).Text,
			Step: &ToolPlanStep{
				Name:      "seed_entities",
				ToolName:  "kg_get_entity",
				Arguments: map[string]any{"entity_ids": p.entityIDs},
				DependsOn: -1,
				Rationale: fmt.Sprintf("Ground the briefing on the proposal's %s (batched)", describeEntities(p.entityIDs)),
			},
		}, nil
	}

	// Seeds done (or none). Defensive: no inner planner → finalize on
	// seed-only grounding.
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
			"incidents (kg_vector). Do not re-fetch the seed entities. To find "+
			"records RELATED to the seed entity (work orders, maintenance history, "+
			"connected equipment), prefer kg_traverse FROM the seed entity rather "+
			"than guessing an entity_type for kg_query_entities — entity types are "+
			"tenant-specific and a guessed type name will fail.]",
		describeEntities(ids))
}

// augmentInvestigationQueryGrounded is the planning directive for the skip path
// (OGA-419): the seeds were grounded earlier in this thread and their details
// are already in the injected per-agent history, so the LLM should NOT re-fetch
// them unless it needs current live values.
func augmentInvestigationQueryGrounded(query string, ids []string) string {
	if len(ids) == 0 {
		return query
	}
	return query + fmt.Sprintf(
		"\n\n[Planning note: %s were already retrieved earlier in this "+
			"investigation thread and their details are in the conversation history "+
			"above. Do NOT re-fetch them with kg_get_entity unless you specifically "+
			"need their CURRENT live values. Focus on gathering ADDITIONAL evidence "+
			"to answer the operator — recent sensor readings/trends (kg_ts_read, "+
			"kg_ts_analyze), related equipment (kg_traverse, kg_query_entities), the "+
			"governing SOP (kg_doc_content), prior work orders, or similar past "+
			"incidents (kg_vector). To find records RELATED to the seed entity (work "+
			"orders, maintenance history, connected equipment), prefer kg_traverse "+
			"FROM the seed entity rather than guessing an entity_type for "+
			"kg_query_entities — entity types are tenant-specific and a guessed type "+
			"name will fail.]",
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
