package agent

import "testing"

func profileWithActions(routing *RoutingDef, ep *EscalationPolicyDef) *DomainAgentProfile {
	return &DomainAgentProfile{
		Name: "fm-ops",
		ProactiveReasoning: &ProactiveConfig{
			Routing:          routing,
			EscalationPolicy: ep,
			Actions: []ActionDef{
				{Name: "create_work_order", HumanActionMode: "approval", RiskLevel: "medium",
					Entity: EntityDef{Type: EntityTypeExisting, Name: "WorkOrder"}},
			},
		},
	}
}

func TestValidateActions_RoutingRequiredWhenActionsPresent(t *testing.T) {
	// No routing → OGA-DKIT-VAL-1040.
	err := validateActions(profileWithActions(nil, nil))
	if err == nil {
		t.Fatal("expected routing-required error")
	}
	if got := codeOf(t, err); got != ErrCodeActionRoutingRequired {
		t.Errorf("code = %s, want %s", got, ErrCodeActionRoutingRequired)
	}

	// Routing with no target → still 1040.
	if err := validateActions(profileWithActions(&RoutingDef{}, nil)); err == nil ||
		codeOf(t, err) != ErrCodeActionRoutingRequired {
		t.Errorf("empty routing should fail with %s, got %v", ErrCodeActionRoutingRequired, err)
	}

	// Valid routing → passes.
	if err := validateActions(profileWithActions(&RoutingDef{TargetRoles: []string{"fm_operator"}}, nil)); err != nil {
		t.Errorf("valid routing should pass: %v", err)
	}
}

func TestValidateActions_EscalationDuration(t *testing.T) {
	routing := &RoutingDef{TargetRoles: []string{"fm_operator"}}

	bad := &EscalationPolicyDef{Timeout: "30 minutes", Routing: RoutingDef{TargetRoles: []string{"fm_manager"}}}
	if err := validateActions(profileWithActions(routing, bad)); err == nil ||
		codeOf(t, err) != ErrCodeActionEscalationDur {
		t.Errorf("bad escalation timeout should fail with %s, got %v", ErrCodeActionEscalationDur, err)
	}

	ok := &EscalationPolicyDef{Timeout: "30m", NotificationHoldWindow: "5s",
		Routing: RoutingDef{TargetRoles: []string{"fm_manager"}}}
	if err := validateActions(profileWithActions(routing, ok)); err != nil {
		t.Errorf("valid escalation policy should pass: %v", err)
	}
}

func TestValidateActions_NoActionsNoRoutingRequired(t *testing.T) {
	// A profile with no actions needs no routing.
	p := &DomainAgentProfile{Name: "x", ProactiveReasoning: &ProactiveConfig{}}
	if err := validateActions(p); err != nil {
		t.Errorf("no-actions profile should pass: %v", err)
	}
}

func TestRoutingDef_ToActionRouting(t *testing.T) {
	r := &RoutingDef{TargetUserID: "op-1", TargetRoles: []string{"fm_operator"}, Channels: []string{"all"}}
	got := r.ToActionRouting()
	if got.TargetUserID != "op-1" || len(got.TargetRoles) != 1 || got.Channels[0] != "all" {
		t.Errorf("unexpected conversion: %+v", got)
	}
	var nilR *RoutingDef
	if nilR.ToActionRouting().HasTarget() {
		t.Error("nil RoutingDef should convert to empty routing")
	}
}
