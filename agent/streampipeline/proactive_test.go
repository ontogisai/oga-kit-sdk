package streampipeline

import (
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

func candidateActions() []agent.ActionDef {
	return []agent.ActionDef{
		{Name: "create_work_order", Description: "create WO", HumanActionMode: "approval", RiskLevel: "medium",
			Entity: agent.EntityDef{Type: agent.EntityTypeExisting, Name: "WorkOrder"}},
		{Name: "log_observation", Description: "log obs", HumanActionMode: "acknowledgement", RiskLevel: "informational",
			Entity: agent.EntityDef{Type: agent.EntityTypeNew, Name: "AgentObservation",
				Schema: map[string]any{
					"type":     "object",
					"required": []any{"observation"},
					"properties": map[string]any{
						"observation": map[string]any{"type": "string"},
					},
				}}},
	}
}

func TestBuildActionDecisionSchema(t *testing.T) {
	schema, err := buildActionDecisionSchema(candidateActions())
	if err != nil {
		t.Fatalf("buildActionDecisionSchema: %v", err)
	}

	// Valid: existing action with permissive payload.
	valid := map[string]any{"action_type": "create_work_order", "payload": map[string]any{"priority": "P2"}, "reasoning": "trend"}
	if err := schema.Validate(valid); err != nil {
		t.Errorf("valid create_work_order decision should pass: %v", err)
	}

	// Valid: no_action with just reasoning.
	if err := schema.Validate(map[string]any{"action_type": "no_action", "reasoning": "nothing warranted"}); err != nil {
		t.Errorf("no_action decision should pass: %v", err)
	}

	// Invalid: unknown action_type matches no branch.
	if err := schema.Validate(map[string]any{"action_type": "teleport", "reasoning": "x"}); err == nil {
		t.Error("unknown action_type should fail oneOf")
	}

	// Invalid: log_observation payload violates its required schema.
	bad := map[string]any{"action_type": "log_observation", "payload": map[string]any{}, "reasoning": "x"}
	if err := schema.Validate(bad); err == nil {
		t.Error("log_observation without required 'observation' should fail")
	}
}

func TestBuildSubmitActionInput(t *testing.T) {
	profile := &agent.DomainAgentProfile{
		Name: "fm-ops",
		ProactiveReasoning: &agent.ProactiveConfig{
			Routing: &agent.RoutingDef{TargetRoles: []string{"fm_operator"}},
			EscalationPolicy: &agent.EscalationPolicyDef{
				Timeout: "30m", NotificationHoldWindow: "5s",
				Routing: agent.RoutingDef{TargetRoles: []string{"fm_manager"}},
			},
		},
	}
	action := &agent.ActionDef{
		Name: "create_work_order", Description: "create WO",
		HumanActionMode: "approval", RiskLevel: "low", AutoApproveTimeout: "5m",
	}
	event := &agent.ProactiveEvent{EventID: "evt-1", EventType: "EntityAnomalyEvent", EntityID: "CH-01"}
	decision := &agent.ActionDecision{ActionType: "create_work_order", Payload: map[string]any{"priority": "P2"}, Reasoning: "trend"}

	in := buildSubmitActionInput(profile, action, event, decision)

	if in.ActionName != "create_work_order" || in.TriggerEventID != "evt-1" {
		t.Errorf("unexpected base fields: %+v", in)
	}
	if in.HumanActionMode != gateway.HumanActionModeApproval || in.RiskLevel != gateway.RiskLevelLow {
		t.Errorf("enum conversion wrong: mode=%s risk=%s", in.HumanActionMode, in.RiskLevel)
	}
	if !in.AutoApproveEligible {
		t.Error("low risk should be auto-approve eligible")
	}
	if in.AutoApproveTimeout.String() != "5m0s" {
		t.Errorf("auto_approve_timeout = %s, want 5m0s", in.AutoApproveTimeout)
	}
	if in.EscalationTimeout.String() != "30m0s" || in.NotificationHoldWindow.String() != "5s" {
		t.Errorf("escalation durations wrong: to=%s hold=%s", in.EscalationTimeout, in.NotificationHoldWindow)
	}
	if len(in.Routing.TargetRoles) != 1 || in.Routing.TargetRoles[0] != "fm_operator" {
		t.Errorf("primary routing wrong: %+v", in.Routing)
	}
	if len(in.EscalationRouting.TargetRoles) != 1 || in.EscalationRouting.TargetRoles[0] != "fm_manager" {
		t.Errorf("escalation routing wrong: %+v", in.EscalationRouting)
	}
	// Description falls back to the action's when the decision omits it.
	if in.Description != "create WO" {
		t.Errorf("description fallback wrong: %q", in.Description)
	}
}

func TestBuildSubmitActionInput_MediumRiskNotEligible(t *testing.T) {
	profile := &agent.DomainAgentProfile{
		ProactiveReasoning: &agent.ProactiveConfig{Routing: &agent.RoutingDef{TargetRoles: []string{"r"}}},
	}
	action := &agent.ActionDef{Name: "x", HumanActionMode: "approval", RiskLevel: "medium"}
	in := buildSubmitActionInput(profile, action, &agent.ProactiveEvent{}, &agent.ActionDecision{ActionType: "x"})
	if in.AutoApproveEligible {
		t.Error("medium risk must not be auto-approve eligible")
	}
}
