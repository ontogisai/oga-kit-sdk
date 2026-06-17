package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// fakeStreamPlanner is a test double for the inner LLM planner.
type fakeStreamPlanner struct {
	plan     *ToolPlan
	err      error
	gotQuery string
	gotTools []string
	calls    int
}

func (f *fakeStreamPlanner) Plan(_ context.Context, query string, tools []string) (*ToolPlan, *PlanNarrative, error) {
	f.calls++
	f.gotQuery = query
	f.gotTools = tools
	return f.plan, &PlanNarrative{Text: "inner"}, f.err
}

func TestInvestigationLLMPlanner_SeedThenLLM(t *testing.T) {
	inner := &fakeStreamPlanner{
		plan: &ToolPlan{Steps: []ToolPlanStep{
			{Name: "sop", ToolName: "kg_doc_content", Arguments: map[string]any{"query": "chiller COP SOP"}, DependsOn: -1},
			// Dependent LLM step: depends on the inner plan's step 0 (the SOP).
			{Name: "history", ToolName: "kg_query_entities", Arguments: map[string]any{"entity_type": "WorkOrder"}, DependsOn: 0},
		}},
	}
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, inner)
	plan, narrative, err := p.Plan(context.Background(), "Is this justified?", []string{"kg_get_entity", "kg_doc_content", "kg_query_entities"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if narrative == nil || narrative.Text == "" {
		t.Error("expected a non-empty narrative")
	}
	// 1 seed + 2 LLM steps.
	if len(plan.Steps) != 3 {
		t.Fatalf("steps = %d, want 3 (1 seed + 2 LLM)", len(plan.Steps))
	}
	// Seed step is the guaranteed kg_get_entity, DependsOn -1.
	seed := plan.Steps[0]
	if seed.ToolName != "kg_get_entity" || seed.Arguments["entity_id"] != "chiller-1" || seed.DependsOn != -1 {
		t.Errorf("seed step = %+v, want kg_get_entity{chiller-1} DependsOn -1", seed)
	}
	// LLM step 0 had no dependency → stays -1.
	if plan.Steps[1].ToolName != "kg_doc_content" || plan.Steps[1].DependsOn != -1 {
		t.Errorf("step 1 = %+v, want kg_doc_content DependsOn -1", plan.Steps[1])
	}
	// LLM step 1 depended on inner index 0 → offset by len(seed)=1 → 1.
	if plan.Steps[2].ToolName != "kg_query_entities" || plan.Steps[2].DependsOn != 1 {
		t.Errorf("step 2 DependsOn = %d, want 1 (inner 0 + offset 1)", plan.Steps[2].DependsOn)
	}
	// The LLM received the full tool list + the augmentation directive.
	if len(inner.gotTools) != 3 {
		t.Errorf("inner tools = %v, want full toolbox passed through", inner.gotTools)
	}
	if !strings.Contains(inner.gotQuery, "ADDITIONAL tools") || !strings.Contains(inner.gotQuery, "Is this justified?") {
		t.Errorf("inner query missing augmentation or original text:\n%s", inner.gotQuery)
	}
}

func TestInvestigationLLMPlanner_LLMFailDegradesToSeed(t *testing.T) {
	for _, inner := range []*fakeStreamPlanner{
		{err: context.DeadlineExceeded}, // planning error
		{plan: &ToolPlan{}},             // empty plan
		{plan: nil},                     // nil plan
	} {
		p := NewInvestigationLLMPlanner([]string{"chiller-1", "ahu-2"}, inner)
		plan, _, err := p.Plan(context.Background(), "q", nil)
		if err != nil {
			t.Fatalf("Plan should not error on inner failure: %v", err)
		}
		// Seed-only: 2 kg_get_entity steps, both grounding the entities.
		if len(plan.Steps) != 2 {
			t.Fatalf("steps = %d, want 2 (seed-only on inner failure)", len(plan.Steps))
		}
		for i, s := range plan.Steps {
			if s.ToolName != "kg_get_entity" {
				t.Errorf("seed step %d = %s, want kg_get_entity", i, s.ToolName)
			}
		}
	}
}

func TestInvestigationLLMPlanner_NilInnerPlanner(t *testing.T) {
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, nil)
	plan, _, err := p.Plan(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].ToolName != "kg_get_entity" {
		t.Fatalf("nil inner planner should yield seed-only plan, got %+v", plan.Steps)
	}
}

func TestInvestigationLLMPlanner_DedupAndCap(t *testing.T) {
	ids := []string{"a", "a", "", "b", "c", "d", "e", "f", "g"} // 7 distinct non-empty, cap 5
	inner := &fakeStreamPlanner{plan: &ToolPlan{}}              // empty → seed-only
	p := NewInvestigationLLMPlanner(ids, inner)
	plan, _, err := p.Plan(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Capped at maxInvestigationEntities seed steps.
	if len(plan.Steps) != maxInvestigationEntities {
		t.Fatalf("seed steps = %d, want %d (cap)", len(plan.Steps), maxInvestigationEntities)
	}
	if plan.Steps[0].Arguments["entity_id"] != "a" {
		t.Errorf("first entity = %v, want a (dedup, blanks dropped)", plan.Steps[0].Arguments["entity_id"])
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

// seqStreamPlanner returns a different (plan, err) per call so tests can model
// "empty first plan, then a corrective re-plan" sequences (OGA-398).
type seqStreamPlanner struct {
	plans   []*ToolPlan
	errs    []error
	calls   int
	queries []string
}

func (f *seqStreamPlanner) Plan(_ context.Context, query string, _ []string) (*ToolPlan, *PlanNarrative, error) {
	i := f.calls
	f.calls++
	f.queries = append(f.queries, query)
	var pl *ToolPlan
	if i < len(f.plans) {
		pl = f.plans[i]
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	return pl, &PlanNarrative{Text: "inner"}, err
}

func TestInvestigationLLMPlanner_EmptyThenCorrectiveReplanSucceeds(t *testing.T) {
	inner := &seqStreamPlanner{plans: []*ToolPlan{
		{}, // 1st: empty → triggers corrective re-plan
		{Steps: []ToolPlanStep{{Name: "sop", ToolName: "kg_doc_content", DependsOn: -1}}}, // 2nd: real evidence
	}}
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, inner)
	plan, _, err := p.Plan(context.Background(), "Is this justified?", []string{"kg_doc_content"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner planner calls = %d, want exactly 2 (one corrective re-plan)", inner.calls)
	}
	// seed + the corrective plan's step.
	if len(plan.Steps) != 2 || plan.Steps[0].ToolName != "kg_get_entity" || plan.Steps[1].ToolName != "kg_doc_content" {
		t.Fatalf("steps = %+v, want [kg_get_entity, kg_doc_content]", plan.Steps)
	}
	// The corrective turn used the forceful directive.
	if !strings.Contains(inner.queries[1], "You returned NO tool calls") {
		t.Errorf("corrective query missing forceful directive:\n%s", inner.queries[1])
	}
}

func TestInvestigationLLMPlanner_EmptyTwiceDegradesWithLimitedEvidence(t *testing.T) {
	inner := &seqStreamPlanner{plans: []*ToolPlan{{}, {}}} // empty both times
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, inner)
	plan, narrative, err := p.Plan(context.Background(), "q", []string{"kg_doc_content"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("inner calls = %d, want 2 (one corrective re-plan, then degrade)", inner.calls)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].ToolName != "kg_get_entity" {
		t.Fatalf("degraded plan = %+v, want seed-only kg_get_entity", plan.Steps)
	}
	if narrative == nil || !strings.Contains(narrative.Text, "Limited evidence") {
		t.Errorf("degraded narrative must flag limited evidence, got %q", narrative.Text)
	}
}

func TestInvestigationLLMPlanner_PlanningErrorDoesNotReplan(t *testing.T) {
	inner := &seqStreamPlanner{errs: []error{context.DeadlineExceeded}}
	p := NewInvestigationLLMPlanner([]string{"chiller-1"}, inner)
	plan, narrative, err := p.Plan(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Plan should degrade, not error: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1 (a transport/context error is not re-planned)", inner.calls)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].ToolName != "kg_get_entity" {
		t.Fatalf("degraded plan = %+v, want seed-only", plan.Steps)
	}
	if narrative == nil || !strings.Contains(narrative.Text, "Limited evidence") {
		t.Errorf("degraded narrative must flag limited evidence, got %q", narrative.Text)
	}
}

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
