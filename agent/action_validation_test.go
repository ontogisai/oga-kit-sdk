package agent

import (
	"errors"
	"testing"
)

func validAction() ActionDef {
	return ActionDef{
		Name:            "create_work_order",
		Description:     "Create a corrective maintenance work order",
		HumanActionMode: "approval",
		RiskLevel:       "medium",
		Entity:          EntityDef{Type: EntityTypeExisting, Name: "WorkOrder"},
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var ve *ActionValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ActionValidationError, got %T (%v)", err, err)
	}
	return ve.Code
}

func TestValidateAction_Valid(t *testing.T) {
	if err := validateAction(ptr(validAction())); err != nil {
		t.Fatalf("valid action should pass: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestValidateAction_ErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ActionDef)
		want string
	}{
		{"bad human_action_mode", func(a *ActionDef) { a.HumanActionMode = "maybe" }, ErrCodeActionHumanMode},
		{"bad risk_level", func(a *ActionDef) { a.RiskLevel = "extreme" }, ErrCodeActionRiskLevel},
		{"bad entity.type", func(a *ActionDef) { a.Entity.Type = "imaginary" }, ErrCodeActionEntityType},
		{"schema required for new", func(a *ActionDef) {
			a.Entity = EntityDef{Type: EntityTypeNew, Name: "AgentObservation"}
		}, ErrCodeActionSchemaRequired},
		{"invalid schema", func(a *ActionDef) {
			a.Entity = EntityDef{Type: EntityTypeNew, Name: "X", Schema: map[string]any{"type": 123}}
		}, ErrCodeActionSchemaInvalid},
		{"external_system required", func(a *ActionDef) {
			a.Entity = EntityDef{Type: EntityTypeExternalReference, Schema: map[string]any{"type": "object"}}
			a.Executor = &ExecutorDef{Tool: "sap_create"}
		}, ErrCodeActionExternalSystem},
		{"executor required", func(a *ActionDef) {
			a.Entity = EntityDef{Type: EntityTypeExternalReference, ExternalSystem: "sap", Schema: map[string]any{"type": "object"}}
		}, ErrCodeActionExecutorRequired},
		{"bad rel source", func(a *ActionDef) {
			a.Relationships = []RelDef{{Source: "bogus.x", EdgeType: "AFFECTS", Direction: "outgoing"}}
		}, ErrCodeActionRelSource},
		{"bad rel direction", func(a *ActionDef) {
			a.Relationships = []RelDef{{Source: "event.entity_id", EdgeType: "AFFECTS", Direction: "sideways"}}
		}, ErrCodeActionRelDirection},
		{"bad auto_approve_timeout", func(a *ActionDef) { a.AutoApproveTimeout = "5 fortnights" }, ErrCodeActionAutoApprove},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validAction()
			tc.mut(&a)
			err := validateAction(&a)
			if err == nil {
				t.Fatalf("expected error %s, got nil", tc.want)
			}
			if got := codeOf(t, err); got != tc.want {
				t.Errorf("code = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidateAction_ExternalReferenceValid(t *testing.T) {
	a := ActionDef{
		Name:            "create_sap_wo",
		Description:     "Create a work order in SAP",
		HumanActionMode: "approval",
		RiskLevel:       "high",
		Entity: EntityDef{
			Type:           EntityTypeExternalReference,
			ExternalSystem: "sap",
			Schema:         map[string]any{"type": "object", "required": []any{"equipment_id"}},
		},
		Executor: &ExecutorDef{Tool: "sap_create_wo"},
	}
	if err := validateAction(&a); err != nil {
		t.Fatalf("valid external_reference action should pass: %v", err)
	}
}
