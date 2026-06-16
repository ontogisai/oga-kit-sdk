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
					Outcome: OutcomeDef{KnowledgeGraphEntity: &KnowledgeGraphEntityDef{Type: EntityTypeExisting, Name: "WorkOrder"}}},
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

	ok := &EscalationPolicyDef{Timeout: "30m", Routing: RoutingDef{TargetRoles: []string{"fm_manager"}}}
	if err := validateActions(profileWithActions(routing, ok)); err != nil {
		t.Errorf("valid escalation policy should pass: %v", err)
	}
}

func TestValidateActions_RoutingHoldWindow(t *testing.T) {
	// Bad hold window on the primary routing → OGA-DKIT-VAL-1041.
	badRouting := &RoutingDef{TargetRoles: []string{"fm_operator"}, NotificationHoldWindow: "5 secs"}
	if err := validateActions(profileWithActions(badRouting, nil)); err == nil ||
		codeOf(t, err) != ErrCodeActionEscalationDur {
		t.Errorf("bad routing hold window should fail with %s, got %v", ErrCodeActionEscalationDur, err)
	}

	// Valid hold window passes and converts to a duration.
	okRouting := &RoutingDef{TargetRoles: []string{"fm_operator"}, NotificationHoldWindow: "5s"}
	if err := validateActions(profileWithActions(okRouting, nil)); err != nil {
		t.Errorf("valid routing hold window should pass: %v", err)
	}
	if got := okRouting.ToActionRouting().NotificationHoldWindow.String(); got != "5s" {
		t.Errorf("hold window conversion = %s, want 5s", got)
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
	r := &RoutingDef{TargetUsers: []string{"fm@ex.io"}, TargetRoles: []string{"fm_operator"}, Channels: []string{"all"}}
	got := r.ToActionRouting()
	if len(got.TargetRoles) != 1 || got.TargetRoles[0] != "fm_operator" || got.Channels[0] != "all" {
		t.Errorf("unexpected conversion: %+v", got)
	}
	if len(got.TargetUsers) != 1 || got.TargetUsers[0] != "fm@ex.io" {
		t.Errorf("ToActionRouting must propagate target_users (emails), got %+v", got.TargetUsers)
	}
	// By-user-id routing is not a kit-authored concept: ToActionRouting never
	// propagates a user id (the field is a rejected catch-field; see
	// TestValidateActions_RejectsDirectUserRouting).
	if got.TargetUserID != "" {
		t.Errorf("ToActionRouting must not propagate a by-id target, got %q", got.TargetUserID)
	}
	var nilR *RoutingDef
	if nilR.ToActionRouting().HasTarget() {
		t.Error("nil RoutingDef should convert to empty routing")
	}
}

// TestValidateActions_TargetUsersAllowed asserts a kit may declare email
// distribution addresses (target_users) — emails are portable, unlike a user id.
func TestValidateActions_TargetUsersAllowed(t *testing.T) {
	r := &RoutingDef{TargetUsers: []string{"fm-desk@ex.io"}}
	if err := validateActions(profileWithActions(r, nil)); err != nil {
		t.Errorf("target_users (email) routing should be allowed: %v", err)
	}
}

// TestValidateActions_RejectsDirectUserRouting asserts the kit-author guardrail:
// a manifest routing block that addresses a recipient directly by user id is
// rejected with OGA-DKIT-VAL-1050 (by-id is non-portable; kits declare
// target_users / target_roles / target_groups). The rejected catch-fields
// (target_user_id / user_id / operator_id) exist solely to surface this code
// instead of a generic decode error. Covers both primary and escalation routing.
func TestValidateActions_RejectsDirectUserRouting(t *testing.T) {
	cases := []struct {
		name    string
		routing *RoutingDef
		esc     *EscalationPolicyDef
	}{
		{"primary target_user_id", &RoutingDef{TargetUserID: "user-1"}, nil},
		{"primary user_id", &RoutingDef{UserID: "user-1"}, nil},
		{"primary operator_id (legacy)", &RoutingDef{OperatorID: "op-1"}, nil},
		{"escalation target_user_id",
			&RoutingDef{TargetRoles: []string{"fm_operator"}},
			&EscalationPolicyDef{Routing: RoutingDef{TargetUserID: "user-2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateActions(profileWithActions(tc.routing, tc.esc))
			if err == nil {
				t.Fatalf("by-user-id routing should be rejected")
			}
			if got := codeOf(t, err); got != ErrCodeActionRoutingDirectUser {
				t.Errorf("code = %s, want %s", got, ErrCodeActionRoutingDirectUser)
			}
		})
	}
}
