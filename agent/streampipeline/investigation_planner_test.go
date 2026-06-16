package streampipeline

import (
	"context"
	"encoding/json"
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

func TestInvestigationEntityIDsFromMessage_ContextJSON(t *testing.T) {
	ic := map[string]any{
		"proposal_id":        "prop-1",
		"agent_id":           "sgac1.fm-operations-agent",
		"trigger_entity_ids": []string{"chiller-1", "ahu-2"},
	}
	raw, _ := json.Marshal(ic)
	m := &agent.Message{Metadata: map[string]any{
		metadataKeyInvestigationContext: string(raw),
	}}
	got := investigationEntityIDsFromMessage(m)
	if len(got) != 2 || got[0] != "chiller-1" || got[1] != "ahu-2" {
		t.Errorf("got %v, want [chiller-1 ahu-2]", got)
	}
}

func TestInvestigationEntityIDsFromMessage_DirectArray(t *testing.T) {
	// JSON-decoded metadata yields []any, not []string — exercise coercion.
	m := &agent.Message{Metadata: map[string]any{
		metadataKeyTriggerEntityIDs: []any{"e1", "", "e2", 42},
	}}
	got := investigationEntityIDsFromMessage(m)
	if len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Errorf("got %v, want [e1 e2] (blanks + non-strings dropped)", got)
	}
}

func TestInvestigationEntityIDsFromMessage_None(t *testing.T) {
	cases := []*agent.Message{
		nil,
		{Metadata: nil},
		{Metadata: map[string]any{"intent": "investigation"}}, // no ids
		{Metadata: map[string]any{metadataKeyInvestigationContext: "{not json"}},
		{Metadata: map[string]any{metadataKeyInvestigationContext: `{"proposal_id":"p"}`}}, // no trigger_entity_ids
	}
	for i, m := range cases {
		if got := investigationEntityIDsFromMessage(m); got != nil {
			t.Errorf("case %d: got %v, want nil", i, got)
		}
	}
}
