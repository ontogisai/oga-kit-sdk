package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

func TestInvestigationGroundingPlanner_PerEntitySteps(t *testing.T) {
	p := NewInvestigationGroundingPlanner([]string{"chiller-1"})
	plan, narrative, err := p.Plan(context.Background(), "investigate", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if narrative == nil || narrative.Text == "" {
		t.Error("expected a non-empty narrative")
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (kg_get_entity + kg_traverse)", len(plan.Steps))
	}
	get := plan.Steps[0]
	if get.ToolName != "kg_get_entity" || get.Arguments["entity_id"] != "chiller-1" {
		t.Errorf("step 0 = %+v, want kg_get_entity{entity_id: chiller-1}", get)
	}
	if get.Required {
		t.Error("grounding steps must be non-Required (degrade gracefully)")
	}
	trav := plan.Steps[1]
	if trav.ToolName != "kg_traverse" || trav.Arguments["start_entity_id"] != "chiller-1" {
		t.Errorf("step 1 = %+v, want kg_traverse{start_entity_id: chiller-1}", trav)
	}
	if trav.Arguments["max_depth"] != 1 {
		t.Errorf("kg_traverse max_depth = %v, want 1", trav.Arguments["max_depth"])
	}
}

func TestInvestigationGroundingPlanner_DedupAndCap(t *testing.T) {
	ids := []string{"a", "a", "", "b", "c", "d", "e", "f", "g"} // 7 distinct non-empty, cap 5
	p := NewInvestigationGroundingPlanner(ids)
	plan, _, err := p.Plan(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// 5 entities × 2 steps.
	if len(plan.Steps) != maxInvestigationEntities*2 {
		t.Fatalf("steps = %d, want %d (cap %d × 2)", len(plan.Steps), maxInvestigationEntities*2, maxInvestigationEntities)
	}
	// First entity is "a" exactly once (dedup); "" dropped.
	if plan.Steps[0].Arguments["entity_id"] != "a" {
		t.Errorf("first entity = %v, want a", plan.Steps[0].Arguments["entity_id"])
	}
}

func TestInvestigationGroundingPlanner_Empty(t *testing.T) {
	for _, ids := range [][]string{nil, {}, {"", ""}} {
		p := NewInvestigationGroundingPlanner(ids)
		plan, _, err := p.Plan(context.Background(), "", nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(plan.Steps) != 0 {
			t.Errorf("ids=%v: steps = %d, want 0 (empty → plain-answer fallback)", ids, len(plan.Steps))
		}
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
		"create_work_order", "Create a PM work order", "PM work order dispatched",
		"COP dropped to 0.49", "Do not propose a different action", "Why this chiller?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enriched query missing %q\n--- got ---\n%s", want, out)
		}
	}

	// No anchoring fields → query unchanged.
	if got := enrichQueryWithInvestigationContext("plain", &investigationContext{}); got != "plain" {
		t.Errorf("empty context should pass through: got %q", got)
	}
}
