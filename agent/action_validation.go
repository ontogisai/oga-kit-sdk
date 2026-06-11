package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Entity type and direction constants for action declarations.
const (
	EntityTypeExisting = "existing"
	EntityTypeNew      = "new"

	relDirectionOutgoing = "outgoing"
	relDirectionIncoming = "incoming"
)

// validateActions runs the full action-declaration validation pass over a
// loaded profile, returning the first ActionValidationError encountered. A
// profile with no proactive actions passes trivially.
func validateActions(p *DomainAgentProfile) error {
	actions := p.Actions()
	for i := range actions {
		if err := validateAction(&p.ProactiveReasoning.Actions[i]); err != nil {
			return err
		}
	}
	if len(actions) > 0 {
		if err := validateProactiveRouting(p.ProactiveReasoning); err != nil {
			return err
		}
	}
	return nil
}

// validateProactiveRouting enforces that a profile declaring actions also
// declares a primary routing target, and that any routing/escalation durations
// parse. Called only when the profile has at least one action.
func validateProactiveRouting(pr *ProactiveConfig) error {
	if pr == nil || !pr.Routing.HasTarget() {
		return newActionValidationError(ErrCodeActionRoutingRequired, "", "proactive_reasoning.routing",
			"required (at least one of target_user_id/target_roles/target_groups) when actions are declared")
	}
	if pr.Routing.NotificationHoldWindow != "" {
		if _, err := time.ParseDuration(pr.Routing.NotificationHoldWindow); err != nil {
			return newActionValidationError(ErrCodeActionEscalationDur, "", "proactive_reasoning.routing.notification_hold_window",
				fmt.Sprintf("not a valid Go duration: %v", err))
		}
	}
	if pr.EscalationPolicy != nil && pr.EscalationPolicy.Timeout != "" {
		if _, err := time.ParseDuration(pr.EscalationPolicy.Timeout); err != nil {
			return newActionValidationError(ErrCodeActionEscalationDur, "", "proactive_reasoning.escalation_policy.timeout",
				fmt.Sprintf("not a valid Go duration: %v", err))
		}
	}
	return nil
}

//nolint:gocyclo // sequential field checks; flat is clearer than extracted helpers here
func validateAction(a *ActionDef) error {
	if a.HumanActionMode != "approval" && a.HumanActionMode != "acknowledgement" {
		return newActionValidationError(ErrCodeActionHumanMode, a.Name, "human_action_mode",
			fmt.Sprintf("must be 'approval' or 'acknowledgement', got %q", a.HumanActionMode))
	}
	switch a.RiskLevel {
	case "informational", "low", "medium", "high":
	default:
		return newActionValidationError(ErrCodeActionRiskLevel, a.Name, "risk_level",
			fmt.Sprintf("must be informational|low|medium|high, got %q", a.RiskLevel))
	}

	switch a.Outcome.Mode() {
	case OutcomeKnowledgeGraphEntity:
		if err := validateKnowledgeGraphEntity(a, a.Outcome.KnowledgeGraphEntity); err != nil {
			return err
		}
	case OutcomeExternalSystemRecord:
		if err := validateExternalSystemRecord(a, a.Outcome.ExternalSystemRecord); err != nil {
			return err
		}
	default:
		return newActionValidationError(ErrCodeActionOutcomeMode, a.Name, "outcome",
			"must set exactly one of knowledge_graph_entity | external_system_record")
	}

	if a.AutoApproveTimeout != "" {
		if _, err := time.ParseDuration(a.AutoApproveTimeout); err != nil {
			return newActionValidationError(ErrCodeActionAutoApprove, a.Name, "auto_approve_timeout",
				fmt.Sprintf("not a valid Go duration: %v", err))
		}
	}
	return nil
}

// validateKnowledgeGraphEntity validates a knowledge_graph_entity outcome:
// type ∈ {existing,new}, schema required+valid for new (valid if present for
// existing), relationships well-formed, and an optional integration.
func validateKnowledgeGraphEntity(a *ActionDef, kg *KnowledgeGraphEntityDef) error {
	const fp = "outcome.knowledge_graph_entity"
	switch kg.Type {
	case EntityTypeExisting, EntityTypeNew:
	default:
		return newActionValidationError(ErrCodeActionEntityType, a.Name, fp+".type",
			fmt.Sprintf("must be existing|new, got %q", kg.Type))
	}
	if kg.Name == "" {
		return newActionValidationError(ErrCodeActionEntityType, a.Name, fp+".name", "required")
	}
	if kg.Type == EntityTypeNew && len(kg.Schema) == 0 {
		return newActionValidationError(ErrCodeActionSchemaRequired, a.Name, fp+".schema",
			"required when type=new")
	}
	if len(kg.Schema) > 0 {
		if err := compileActionSchema(kg.Schema); err != nil {
			return newActionValidationError(ErrCodeActionSchemaInvalid, a.Name, fp+".schema",
				fmt.Sprintf("not a valid JSON Schema 2020-12: %v", err))
		}
	}
	if err := validateRelationships(a, kg.Relationships); err != nil {
		return err
	}
	if kg.Integration != nil {
		return validateIntegration(a, kg.Integration, fp+".integration")
	}
	return nil
}

// validateExternalSystemRecord validates an external_system_record outcome:
// system + schema required, and a required integration.
func validateExternalSystemRecord(a *ActionDef, ext *ExternalSystemRecordDef) error {
	const fp = "outcome.external_system_record"
	if ext.System == "" {
		return newActionValidationError(ErrCodeActionExternalSystem, a.Name, fp+".system", "required")
	}
	if len(ext.Schema) == 0 {
		return newActionValidationError(ErrCodeActionSchemaRequired, a.Name, fp+".schema",
			"required for external_system_record")
	}
	if err := compileActionSchema(ext.Schema); err != nil {
		return newActionValidationError(ErrCodeActionSchemaInvalid, a.Name, fp+".schema",
			fmt.Sprintf("not a valid JSON Schema 2020-12: %v", err))
	}
	if ext.Integration == nil {
		return newActionValidationError(ErrCodeActionExecutorRequired, a.Name, fp+".integration",
			"required for external_system_record")
	}
	return validateIntegration(a, ext.Integration, fp+".integration")
}

// validateIntegration enforces that an integration declares a tool and maps
// external_record_id from the tool result (the searchable correlation key).
func validateIntegration(a *ActionDef, integ *IntegrationDef, fieldPath string) error {
	if integ.Tool == "" {
		return newActionValidationError(ErrCodeActionExecutorRequired, a.Name, fieldPath+".tool", "required")
	}
	if integ.ResultMapping[externalRecordIDKey] == "" {
		return newActionValidationError(ErrCodeActionExternalRecordID, a.Name,
			fieldPath+".result_mapping.external_record_id",
			"required when an integration is present (maps a tool-result field to the external record id)")
	}
	return nil
}

// externalRecordIDKey is the required ExternalSystemRecord column every
// integration must map from its tool result.
const externalRecordIDKey = "external_record_id"

// validateRelationships checks each relationship's source prefix and direction.
func validateRelationships(a *ActionDef, rels []RelDef) error {
	for j := range rels {
		r := rels[j]
		if !strings.HasPrefix(r.Source, "event.") && !strings.HasPrefix(r.Source, "payload.") {
			return newActionValidationError(ErrCodeActionRelSource, a.Name,
				fmt.Sprintf("relationships[%d].source", j),
				fmt.Sprintf("must start with 'event.' or 'payload.', got %q", r.Source))
		}
		if r.Direction != relDirectionOutgoing && r.Direction != relDirectionIncoming {
			return newActionValidationError(ErrCodeActionRelDirection, a.Name,
				fmt.Sprintf("relationships[%d].direction", j),
				fmt.Sprintf("must be outgoing|incoming, got %q", r.Direction))
		}
	}
	return nil
}

// compileActionSchema verifies that a kit-supplied entity.schema is a valid
// JSON Schema 2020-12 document. It compiles the schema with the default
// 2020-12 dialect; a compile error means the schema is malformed.
func compileActionSchema(schema map[string]any) error {
	c := jsonschema.NewCompiler()
	const url = "mem://oga/action-entity-schema.json"
	if err := c.AddResource(url, schema); err != nil {
		return err
	}
	if _, err := c.Compile(url); err != nil {
		return err
	}
	return nil
}
