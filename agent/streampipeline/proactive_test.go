package streampipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

func candidateActions() []agent.ActionDef {
	return []agent.ActionDef{
		{Name: "create_work_order", Description: "create WO", HumanActionMode: "approval", RiskLevel: "medium",
			Outcome: agent.OutcomeDef{KnowledgeGraphEntity: &agent.KnowledgeGraphEntityDef{Type: agent.EntityTypeExisting, Name: "WorkOrder"}}},
		{Name: "log_observation", Description: "log obs", HumanActionMode: "acknowledgement", RiskLevel: "informational",
			Outcome: agent.OutcomeDef{KnowledgeGraphEntity: &agent.KnowledgeGraphEntityDef{Type: agent.EntityTypeNew, Name: "AgentObservation",
				Schema: map[string]any{
					"type":     "object",
					"required": []any{"observation"},
					"properties": map[string]any{
						"observation": map[string]any{"type": "string"},
					},
				}}}},
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
			Routing: &agent.RoutingDef{TargetRoles: []string{"fm_operator"}, NotificationHoldWindow: "5s"},
			EscalationPolicy: &agent.EscalationPolicyDef{
				Timeout: "30m",
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

	if in.ActionName != "create_work_order" || in.TargetEventID != "evt-1" {
		t.Errorf("unexpected base fields: %+v", in)
	}
	if in.TargetEntityID != "CH-01" {
		t.Errorf("target_entity_id = %q, want CH-01", in.TargetEntityID)
	}
	if len(in.TriggerEventIDs) != 1 || in.TriggerEventIDs[0] != "evt-1" {
		t.Errorf("trigger_event_ids = %v, want [evt-1]", in.TriggerEventIDs)
	}
	if len(in.TriggerEntityIDs) != 1 || in.TriggerEntityIDs[0] != "CH-01" {
		t.Errorf("trigger_entity_ids = %v, want [CH-01]", in.TriggerEntityIDs)
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
	if in.EscalationTimeout.String() != "30m0s" {
		t.Errorf("escalation timeout wrong: %s", in.EscalationTimeout)
	}
	if in.Routing.NotificationHoldWindow.String() != "5s" {
		t.Errorf("primary routing hold window = %s, want 5s", in.Routing.NotificationHoldWindow)
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

// TestProactiveBudget verifies the detached reasoning timeout derivation
// (OGA-343): sum of context-gather + reasoning timeouts plus SubmitAction
// headroom, with a generous fallback when the profile leaves them unset.
func TestProactiveBudget(t *testing.T) {
	const fallback = 120 * time.Second

	t.Run("nil profile falls back", func(t *testing.T) {
		if got := proactiveBudget(nil); got != fallback {
			t.Errorf("proactiveBudget(nil) = %v, want %v", got, fallback)
		}
	})

	t.Run("nil proactive_reasoning falls back", func(t *testing.T) {
		if got := proactiveBudget(&agent.DomainAgentProfile{}); got != fallback {
			t.Errorf("proactiveBudget(no reasoning) = %v, want %v", got, fallback)
		}
	})

	t.Run("unset timeouts fall back", func(t *testing.T) {
		p := &agent.DomainAgentProfile{ProactiveReasoning: &agent.ProactiveConfig{}}
		if got := proactiveBudget(p); got != fallback {
			t.Errorf("proactiveBudget(empty timeouts) = %v, want %v", got, fallback)
		}
	})

	t.Run("sum plus headroom", func(t *testing.T) {
		p := &agent.DomainAgentProfile{ProactiveReasoning: &agent.ProactiveConfig{
			ContextGatherTimeout: "15s",
			ReasoningTimeout:     "30s",
		}}
		want := 15*time.Second + 30*time.Second + 30*time.Second // + SubmitAction headroom
		if got := proactiveBudget(p); got != want {
			t.Errorf("proactiveBudget(15s+30s) = %v, want %v", got, want)
		}
	})
}

// TestProactiveAssemblyPrompt_IncludesPayloadSchema verifies the assembly
// prompt renders each candidate action's declared payload schema inline so the
// reasoning LLM knows the required fields + enum constraints it must satisfy
// (OGA-343). Without this the LLM guesses payload fields and RunSync's post-hoc
// schema validation rejects the output.
func TestProactiveAssemblyPrompt_IncludesPayloadSchema(t *testing.T) {
	profile := &agent.DomainAgentProfile{
		Name:               "fm-ops",
		ProactiveReasoning: &agent.ProactiveConfig{SystemPrompt: "You are FM ops."},
	}
	prompt := proactiveAssemblyPrompt(profile, candidateActions())

	// The kit system prompt is prepended.
	if !strings.Contains(prompt, "You are FM ops.") {
		t.Error("assembly prompt should include the kit system prompt")
	}
	// Both actions appear by name.
	if !strings.Contains(prompt, "create_work_order") || !strings.Contains(prompt, "log_observation") {
		t.Errorf("assembly prompt missing candidate action names:\n%s", prompt)
	}
	// log_observation declares a payload schema → its required field must surface.
	if !strings.Contains(prompt, "payload schema") || !strings.Contains(prompt, "observation") {
		t.Errorf("assembly prompt should render log_observation's payload schema:\n%s", prompt)
	}
	// no_action option is always offered.
	if !strings.Contains(prompt, "no_action") {
		t.Errorf("assembly prompt should offer no_action:\n%s", prompt)
	}
	// Guidance to honor required fields + enums.
	if !strings.Contains(prompt, "required field") {
		t.Errorf("assembly prompt should instruct the LLM to include required fields:\n%s", prompt)
	}
}

// TestAckAccepted verifies the fast-ack response shape returned to the Event
// Router before async reasoning begins (OGA-343).
func TestAckAccepted(t *testing.T) {
	resp := agent.AckAccepted(&agent.ProactiveEvent{EventType: "EntityAnomalyEvent"})
	if resp == nil || resp.Message == nil {
		t.Fatal("AckAccepted returned nil response/message")
	}
	if resp.Message.Role != "agent" {
		t.Errorf("role = %q, want agent", resp.Message.Role)
	}
	if len(resp.Message.Parts) == 0 || resp.Message.Parts[0].Text == "" {
		t.Fatal("AckAccepted response has no text part")
	}
	if !strings.Contains(resp.Message.Parts[0].Text, "EntityAnomalyEvent") {
		t.Errorf("ack text should mention the event type, got %q", resp.Message.Parts[0].Text)
	}

	// Nil event must still produce a valid ack.
	if r := agent.AckAccepted(nil); r == nil || r.Message == nil || len(r.Message.Parts) == 0 {
		t.Fatal("AckAccepted(nil) must return a valid ack")
	}
}
