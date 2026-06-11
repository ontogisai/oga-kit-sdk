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
		Outcome: OutcomeDef{
			KnowledgeGraphEntity: &KnowledgeGraphEntityDef{Type: EntityTypeExisting, Name: "WorkOrder"},
		},
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
		{"no outcome mode", func(a *ActionDef) { a.Outcome = OutcomeDef{} }, ErrCodeActionOutcomeMode},
		{"both outcome modes", func(a *ActionDef) {
			a.Outcome.ExternalSystemRecord = &ExternalSystemRecordDef{System: "sap"}
		}, ErrCodeActionOutcomeMode},
		{"bad kg type", func(a *ActionDef) { a.Outcome.KnowledgeGraphEntity.Type = "imaginary" }, ErrCodeActionEntityType},
		{"kg name required", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity = &KnowledgeGraphEntityDef{Type: EntityTypeExisting}
		}, ErrCodeActionEntityType},
		{"schema required for new", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity = &KnowledgeGraphEntityDef{Type: EntityTypeNew, Name: "AgentObservation"}
		}, ErrCodeActionSchemaRequired},
		{"invalid schema", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity = &KnowledgeGraphEntityDef{Type: EntityTypeNew, Name: "X", Schema: map[string]any{"type": 123}}
		}, ErrCodeActionSchemaInvalid},
		{"external_system required", func(a *ActionDef) {
			a.Outcome = OutcomeDef{ExternalSystemRecord: &ExternalSystemRecordDef{
				Schema:      map[string]any{"type": "object"},
				Integration: &IntegrationDef{Tool: "sap_create", ResultMapping: map[string]string{"external_record_id": "id"}},
			}}
		}, ErrCodeActionExternalSystem},
		{"integration required for external", func(a *ActionDef) {
			a.Outcome = OutcomeDef{ExternalSystemRecord: &ExternalSystemRecordDef{
				System: "sap", Schema: map[string]any{"type": "object"},
			}}
		}, ErrCodeActionExecutorRequired},
		{"integration tool required", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity.Integration = &IntegrationDef{ResultMapping: map[string]string{"external_record_id": "id"}}
		}, ErrCodeActionExecutorRequired},
		{"hybrid integration system required", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity.Integration = &IntegrationDef{Tool: "fm_create_wo", ResultMapping: map[string]string{"external_record_id": "id"}}
		}, ErrCodeActionExternalSystem},
		{"external_record_id mapping required", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity.Integration = &IntegrationDef{Tool: "fm_create_wo", System: "contract_wo_mgmt"}
		}, ErrCodeActionExternalRecordID},
		{"bad rel source", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity.Relationships = []RelDef{{Source: "bogus.x", EdgeType: "AFFECTS", Direction: "outgoing"}}
		}, ErrCodeActionRelSource},
		{"bad rel direction", func(a *ActionDef) {
			a.Outcome.KnowledgeGraphEntity.Relationships = []RelDef{{Source: "event.entity_id", EdgeType: "AFFECTS", Direction: "sideways"}}
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

func TestValidateAction_KnowledgeGraphEntityHybridValid(t *testing.T) {
	a := validAction()
	a.Outcome.KnowledgeGraphEntity.Integration = &IntegrationDef{
		System:        "contract_wo_mgmt",
		Tool:          "fm_create_work_order",
		ResultMapping: map[string]string{"external_record_id": "wo_number", "status": "wo_status"},
	}
	a.Outcome.KnowledgeGraphEntity.Relationships = []RelDef{
		{Source: "event.entity_id", EdgeType: "AFFECTS", Direction: "outgoing"},
	}
	if err := validateAction(&a); err != nil {
		t.Fatalf("valid hybrid knowledge_graph_entity action should pass: %v", err)
	}
}

func TestValidateAction_ExternalSystemRecordValid(t *testing.T) {
	a := ActionDef{
		Name:            "create_sap_wo",
		Description:     "Create a work order in SAP",
		HumanActionMode: "approval",
		RiskLevel:       "high",
		Outcome: OutcomeDef{
			ExternalSystemRecord: &ExternalSystemRecordDef{
				System: "sap",
				Schema: map[string]any{"type": "object", "required": []any{"equipment_id"}},
				Integration: &IntegrationDef{
					Tool:          "sap_create_wo",
					ResultMapping: map[string]string{"external_record_id": "id", "status": "state"},
				},
			},
		},
	}
	if err := validateAction(&a); err != nil {
		t.Fatalf("valid external_system_record action should pass: %v", err)
	}
}
