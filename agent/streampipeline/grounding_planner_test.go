package streampipeline

import (
	"context"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

func TestGroundingStrategyPlanner_EmptyProfile(t *testing.T) {
	planner := NewGroundingStrategyPlanner(nil)
	plan, narrative, err := planner.Plan(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil || len(plan.Steps) != 0 {
		t.Errorf("expected empty plan; got %v", plan)
	}
	if narrative == nil || narrative.Text == "" {
		t.Errorf("expected non-empty narrative; got %v", narrative)
	}
}

func TestGroundingStrategyPlanner_ConvertsSteps(t *testing.T) {
	profile := &agent.DomainAgentProfile{
		Name: "FM Operations Agent",
		ProactiveReasoning: &agent.ProactiveConfig{
			GroundingStrategy: []agent.GroundingStep{
				{
					Name:      "domain_sop",
					Tool:      "kg_doc_content",
					Arguments: map[string]any{"query": "{event_type} {entity_type}"},
					Required:  true,
					Condition: "true",
				},
				{
					Name:       "history",
					Tool:       "kg_query_entities",
					Arguments:  map[string]any{"entity_type": "WorkOrder"},
					DependsOn:  "domain_sop",
					MaxResults: 10,
				},
				{
					Name:      "skip_me",
					Tool:      "kg_traverse",
					Arguments: map[string]any{},
					Condition: "false",
				},
			},
		},
	}

	planner := NewGroundingStrategyPlanner(profile)
	plan, narrative, err := planner.Plan(context.Background(), "user query", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Steps) != 3 {
		t.Fatalf("expected 3 steps; got %d", len(plan.Steps))
	}

	// Step 0: domain_sop, Required=true, Condition="true"
	if plan.Steps[0].Name != "domain_sop" {
		t.Errorf("step 0 name = %q", plan.Steps[0].Name)
	}
	if !plan.Steps[0].Required {
		t.Errorf("step 0 should be Required")
	}
	if plan.Steps[0].DependsOn != -1 {
		t.Errorf("step 0 DependsOn = %d, want -1", plan.Steps[0].DependsOn)
	}

	// Step 1: history, DependsOn → resolved to index 0
	if plan.Steps[1].DependsOn != 0 {
		t.Errorf("step 1 DependsOn = %d, want 0", plan.Steps[1].DependsOn)
	}
	if plan.Steps[1].MaxResults != 10 {
		t.Errorf("step 1 MaxResults = %d, want 10", plan.Steps[1].MaxResults)
	}

	// Step 2: skip_me, Condition="false" preserved on the plan (executor evaluates it)
	if plan.Steps[2].Condition != "false" {
		t.Errorf("step 2 Condition = %q, want false", plan.Steps[2].Condition)
	}

	// Narrative should mention persona name.
	if narrative == nil || narrative.Text == "" {
		t.Errorf("narrative empty")
	}
	if !contains(narrative.Text, "FM Operations Agent") {
		t.Errorf("narrative doesn't mention persona: %q", narrative.Text)
	}
}

func TestGroundingStrategyPlanner_DependsOnMissingName(t *testing.T) {
	profile := &agent.DomainAgentProfile{
		Name: "X",
		ProactiveReasoning: &agent.ProactiveConfig{
			GroundingStrategy: []agent.GroundingStep{
				{Name: "a", Tool: "tool_a"},
				{Name: "b", Tool: "tool_b", DependsOn: "nonexistent"},
			},
		},
	}
	planner := NewGroundingStrategyPlanner(profile)
	plan, _, err := planner.Plan(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Unresolved DependsOn defaults to -1 (independent).
	if plan.Steps[1].DependsOn != -1 {
		t.Errorf("DependsOn for missing name = %d, want -1", plan.Steps[1].DependsOn)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
