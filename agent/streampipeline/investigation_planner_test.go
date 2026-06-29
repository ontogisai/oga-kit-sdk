package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// fakeNextPlanner is a Planner test double for the inner LLM planner.
type fakeNextPlanner struct {
	decision *Decision
	err      error
	calls    int
	gotState *PlanState
}

func (f *fakeNextPlanner) Next(_ context.Context, st *PlanState) (*Decision, error) {
	f.calls++
	f.gotState = st
	if f.err != nil {
		return nil, f.err
	}
	return f.decision, nil
}

func TestInvestigationLLMPlanner_BatchedSeedThenDelegate(t *testing.T) {
	inner := &fakeNextPlanner{decision: &Decision{
		Narrative: "inner",
		Step:      &ToolPlanStep{Name: "sop", ToolName: "kg_doc_content", DependsOn: -1},
	}}
	p := NewInvestigationLLMPlanner([]string{"chiller-1", "ahu-2"}, inner)

	// Turn 0 (empty history) → ONE batched kg_get_entity for ALL seeds, with narrative.
	d0, err := p.Next(context.Background(), &PlanState{Query: "Is this justified?"})
	if err != nil {
		t.Fatalf("Next turn 0: %v", err)
	}
	if d0.Done || d0.Step == nil {
		t.Fatalf("turn 0 should yield a seed step, got %+v", d0)
	}
	if d0.Step.ToolName != "kg_get_entity" || d0.Step.DependsOn != -1 {
		t.Errorf("seed step = %+v, want kg_get_entity DependsOn -1", d0.Step)
	}
	ids, ok := d0.Step.Arguments["entity_ids"].([]string)
	if !ok || len(ids) != 2 || ids[0] != "chiller-1" || ids[1] != "ahu-2" {
		t.Errorf("batched seed entity_ids = %v, want [chiller-1 ahu-2]", d0.Step.Arguments["entity_ids"])
	}
	if _, single := d0.Step.Arguments["entity_id"]; single {
		t.Error("batched seed must use entity_ids, not entity_id")
	}
	if d0.Narrative == "" {
		t.Error("expected a non-empty narrative on the seed turn")
	}
	if inner.calls != 0 {
		t.Errorf("inner planner must NOT be called while seeding, calls=%d", inner.calls)
	}

	// Turn 1 (one observation present) → delegate to the inner LLM planner.
	d1, err := p.Next(context.Background(), &PlanState{
		Query:   "Is this justified?",
		History: []ToolStepResult{{ToolName: "kg_get_entity", Success: true, Content: `[{"id":"chiller-1"},{"id":"ahu-2"}]`}},
	})
	if err != nil {
		t.Fatalf("Next turn 1: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner planner calls = %d, want 1 after seeds done", inner.calls)
	}
	if d1.Step == nil || d1.Step.ToolName != "kg_doc_content" {
		t.Errorf("turn 1 should delegate to inner (kg_doc_content), got %+v", d1)
	}
	if inner.gotState == nil || !strings.Contains(inner.gotState.Query, "ADDITIONAL evidence") {
		t.Errorf("inner query missing augmentation:\n%v", inner.gotState)
	}
}

// TestInvestigationLLMPlanner_SkipSeedWhenGrounded covers OGA-419 Option 1: when
// the thread already grounded the seeds recently, the planner skips the fetch
// and delegates to the inner planner on turn 0 with a "grounded earlier" note.
func TestInvestigationLLMPlanner_SkipSeedWhenGrounded(t *testing.T) {
	inner := &fakeNextPlanner{decision: &Decision{
		Step: &ToolPlanStep{Name: "trend", ToolName: "kg_ts_read", DependsOn: -1},
	}}
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, inner, WithSeedsAlreadyGrounded())

	// Turn 0 (empty history) → NO seed fetch; delegate straight to inner.
	d0, err := p.Next(context.Background(), &PlanState{Query: "Why this chiller?"})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner planner should be called on turn 0 when seeds grounded, calls=%d", inner.calls)
	}
	if d0.Step == nil || d0.Step.ToolName != "kg_ts_read" {
		t.Errorf("expected delegation to inner (kg_ts_read), got %+v", d0)
	}
	if inner.gotState == nil ||
		!strings.Contains(inner.gotState.Query, "already retrieved earlier in this") ||
		!strings.Contains(inner.gotState.Query, "CURRENT live values") {
		t.Errorf("inner query missing grounded-skip note:\n%v", inner.gotState)
	}
}

func TestInvestigationLLMPlanner_NilInnerFinalizesAfterSeeds(t *testing.T) {
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, nil)

	// Batched seed turn.
	d0, err := p.Next(context.Background(), &PlanState{Query: "q"})
	if err != nil || d0.Step == nil || d0.Step.ToolName != "kg_get_entity" {
		t.Fatalf("turn 0 should seed, got %+v err=%v", d0, err)
	}
	// After seeds, nil inner → Done.
	d1, err := p.Next(context.Background(), &PlanState{
		Query:   "q",
		History: []ToolStepResult{{ToolName: "kg_get_entity", Success: true, Content: "[]"}},
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !d1.Done {
		t.Errorf("nil inner planner should finalize after seeds, got %+v", d1)
	}
}

func TestInvestigationLLMPlanner_BatchDedupAndCap(t *testing.T) {
	ids := []string{"a", "a", "", "b", "c", "d", "e", "f", "g"} // 7 distinct non-empty, cap 5
	p := NewInvestigationLLMPlanner(ids, nil)

	// One batched seed turn carrying the deduped, capped, blank-stripped ids.
	d, err := p.Next(context.Background(), &PlanState{})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if d.Step == nil || d.Step.ToolName != "kg_get_entity" {
		t.Fatalf("turn 0 not a seed: %+v", d.Step)
	}
	seeded, ok := d.Step.Arguments["entity_ids"].([]string)
	if !ok {
		t.Fatalf("seed step missing entity_ids: %+v", d.Step.Arguments)
	}
	if len(seeded) != maxInvestigationEntities {
		t.Fatalf("seeded ids = %d, want %d (cap)", len(seeded), maxInvestigationEntities)
	}
	if seeded[0] != "a" || seeded[1] != "b" || seeded[2] != "c" {
		t.Errorf("seeds = %v, want dedup + blanks dropped starting [a b c ...]", seeded)
	}
}

func TestInvestigationContextFromMessage_SeedUnion(t *testing.T) {
	// Enriched context: singular target + plural triggers → deduped union seed.
	ic := map[string]any{
		"proposal_id":        "prop-1",
		"agent_id":           "sgac1.fm-operations-agent",
		"target_entity_id":   "chiller-1",
		"target_event_id":    "evt-1",
		"trigger_entity_ids": []string{"chiller-1", "ahu-2"}, // chiller-1 dup of target
		"trigger_event_ids":  []string{"evt-1"},              // dup of target event
	}
	raw, _ := json.Marshal(ic)
	m := &agent.Message{Metadata: map[string]any{
		metadataKeyInvestigationContext: string(raw),
	}}
	got, ok := investigationContextFromMessage(m)
	if !ok {
		t.Fatal("expected investigation context")
	}
	seed := investigationSeedIDs(got)
	// Union, deduped, target-first: chiller-1, evt-1, ahu-2.
	if len(seed) != 3 || seed[0] != "chiller-1" || seed[1] != "evt-1" || seed[2] != "ahu-2" {
		t.Errorf("seed = %v, want [chiller-1 evt-1 ahu-2]", seed)
	}
}

func TestInvestigationContextFromMessage_None(t *testing.T) {
	cases := []*agent.Message{
		nil,
		{Metadata: nil},
		{Metadata: map[string]any{"intent": "investigation"}},                    // no context key
		{Metadata: map[string]any{metadataKeyInvestigationContext: "{not json"}}, // unparseable
		{Metadata: map[string]any{metadataKeyInvestigationContext: ""}},          // empty string
	}
	for i, m := range cases {
		if _, ok := investigationContextFromMessage(m); ok {
			t.Errorf("case %d: expected (nil,false)", i)
		}
	}
}

func TestAppendInvestigationBriefingDirective_AlwaysApplied(t *testing.T) {
	// Even with an empty system prompt (sparse proposal context that rendered
	// no anchoring block), the concise-briefing contract is still applied.
	for _, base := range []string{"", "You are the FM proactive agent."} {
		out := appendInvestigationBriefingDirective(base)
		if !strings.HasPrefix(out, base) {
			t.Errorf("base prompt not preserved as prefix: %q", out)
		}
		for _, want := range []string{
			"concise, succinct, and direct", "at most ~200 words",
			"compact table", "do not speculate",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("directive missing %q\n--- got ---\n%s", want, out)
			}
		}
	}
}

func TestEnrichQueryWithInvestigationContext(t *testing.T) {
	ic := &investigationContext{
		ActionType:      "create_work_order",
		Description:     "Create a PM work order for Chiller CH-36A",
		ExpectedOutcome: "PM work order dispatched within 30 minutes",
		RiskLevel:       "high",
		ReasoningFacts:  []string{"COP dropped to 0.49", "threshold: 0.7"},
	}
	out := enrichQueryWithInvestigationContext("Why this chiller?", ic)
	for _, want := range []string{
		`recommended: "create_work_order"`, // literal quotes — text/template, not html/template (no &#34;)
		"Create a PM work order", "PM work order dispatched",
		"• COP dropped to 0.49", // bullet rendered literally, not escaped
		"Risk level: high", "Do not propose a different action", "Why this chiller?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enriched query missing %q\n--- got ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "&#34;") || strings.Contains(out, "&bull;") {
		t.Errorf("output is HTML-escaped — must use text/template:\n%s", out)
	}

	// No anchoring fields → query unchanged.
	if got := enrichQueryWithInvestigationContext("plain", &investigationContext{}); got != "plain" {
		t.Errorf("empty context should pass through: got %q", got)
	}
}

// seqStreamPlanner and the corrective-re-plan / degraded-narrative tests
// (OGA-398) were removed in OGA-419: under the ReAct loop the inner planner is
// invoked one turn at a time (Next), so the planner no longer pre-composes a
// full plan, performs an empty-plan corrective re-plan, or emits a degraded
// "limited evidence" narrative. The augmented-query directive
// (augmentInvestigationQuery) plus the loop's per-turn re-prompting replace
// that behavior. Seed grounding is still guaranteed (see SeedFirstThenDelegate).

func TestBuildInvestigationPlannerQuery_NoAssemblyConstraints(t *testing.T) {
	ic := &investigationContext{
		ActionType:     "flag_equipment_condition",
		Description:    "Record a degraded condition for CH-36A",
		RiskLevel:      "low",
		ReasoningFacts: []string{"COP dropped to 0.49", "threshold: 0.7"},
	}
	out := buildInvestigationPlannerQuery("Is this justified?", ic)

	// Carries factual proposal context + the operator's question.
	for _, want := range []string{
		`recommended: "flag_equipment_condition"`,
		"Record a degraded condition",
		"COP dropped to 0.49",
		"gather the evidence",
		"Is this justified?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("planner query missing %q\n--- got ---\n%s", want, out)
		}
	}
	// MUST NOT carry the assembly-only constraints (the OGA-398 root cause).
	for _, forbidden := range []string{
		"grounded ONLY in the evidence",
		"Do not propose a different action",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("planner query leaked assembly-only constraint %q — this is the bug being fixed:\n%s", forbidden, out)
		}
	}

	// No anchoring fields → passthrough.
	if got := buildInvestigationPlannerQuery("plain", &investigationContext{}); got != "plain" {
		t.Errorf("empty context should pass through: got %q", got)
	}
}

// TestSeedsAlreadyGrounded covers the handler's metadata flag reader (OGA-419
// Option 1). It must fail safe to "not grounded" on absence/ambiguity so the
// seed re-fetches rather than wrongly skipping.
func TestSeedsAlreadyGrounded(t *testing.T) {
	cases := []struct {
		name string
		m    *agent.Message
		want bool
	}{
		{"nil message", nil, false},
		{"nil metadata", &agent.Message{}, false},
		{"absent key", &agent.Message{Metadata: map[string]any{"x": "y"}}, false},
		{"bool true", &agent.Message{Metadata: map[string]any{metadataKeySeedsGrounded: true}}, true},
		{"bool false", &agent.Message{Metadata: map[string]any{metadataKeySeedsGrounded: false}}, false},
		{"string true", &agent.Message{Metadata: map[string]any{metadataKeySeedsGrounded: "true"}}, true},
		{"string false", &agent.Message{Metadata: map[string]any{metadataKeySeedsGrounded: "false"}}, false},
		{"string other", &agent.Message{Metadata: map[string]any{metadataKeySeedsGrounded: "1"}}, false},
		{"wrong type", &agent.Message{Metadata: map[string]any{metadataKeySeedsGrounded: 1}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := seedsAlreadyGrounded(tc.m); got != tc.want {
				t.Errorf("seedsAlreadyGrounded = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAugmentInvestigation_GraphRelativeNavigationNudge verifies both augment
// directives steer the planner to traverse from the seed entity rather than
// guess a tenant-specific entity_type (OGA-438 part a).
func TestAugmentInvestigation_GraphRelativeNavigationNudge(t *testing.T) {
	ids := []string{"chiller-1"}
	for _, out := range []string{
		augmentInvestigationQuery("Assess the proposal.", ids),
		augmentInvestigationQueryGrounded("Assess the proposal.", ids),
	} {
		if !strings.Contains(out, "kg_traverse FROM the seed entity") {
			t.Errorf("directive missing graph-relative nudge: %q", out)
		}
		if !strings.Contains(out, "entity types are tenant-specific") {
			t.Errorf("directive missing tenant-specific type warning: %q", out)
		}
	}
	// No nudge when there are no seed ids (directive is a no-op).
	if got := augmentInvestigationQuery("q", nil); got != "q" {
		t.Errorf("expected unchanged query with no ids, got %q", got)
	}
}
