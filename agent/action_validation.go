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
	if pr == nil {
		return newActionValidationError(ErrCodeActionRoutingRequired, "", "proactive_reasoning.routing",
			"required (at least one of target_users/target_roles/target_groups) when actions are declared")
	}
	// Reject by-user routing FIRST, so a routing block carrying only a by-user
	// catch-field surfaces OGA-DKIT-VAL-1050 (not the generic routing-required
	// 1041 — HasTarget intentionally ignores the catch-fields).
	if err := validateNoDirectUserRouting(pr.Routing, "proactive_reasoning.routing"); err != nil {
		return err
	}
	if pr.EscalationPolicy != nil {
		if err := validateNoDirectUserRouting(&pr.EscalationPolicy.Routing, "proactive_reasoning.escalation_policy.routing"); err != nil {
			return err
		}
	}
	if !pr.Routing.HasTarget() {
		return newActionValidationError(ErrCodeActionRoutingRequired, "", "proactive_reasoning.routing",
			"required (at least one of target_users/target_roles/target_groups) when actions are declared")
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

// validateNoDirectUserRouting rejects a kit-authored routing block that
// addresses a recipient directly by user id or email. Per-user routing is
// non-portable across tenants — kit manifests may declare only target_roles /
// target_groups. The by-user keys (target_user_id / user_id / operator_id) are
// REJECTED catch-fields on RoutingDef; this surfaces the helpful
// OGA-DKIT-VAL-1050 error instead of a generic decode failure. Programmatic
// gateway construction is unaffected — this guards declarative manifests only.
func validateNoDirectUserRouting(r *RoutingDef, fieldPath string) error {
	if r == nil {
		return nil
	}
	switch {
	case r.TargetUserID != "":
		return newActionValidationError(ErrCodeActionRoutingDirectUser, "", fieldPath+".target_user_id",
			"by-user-id routing is not portable across tenants; use target_users (email) / target_roles / target_groups")
	case r.UserID != "":
		return newActionValidationError(ErrCodeActionRoutingDirectUser, "", fieldPath+".user_id",
			"by-user-id routing is not portable across tenants; use target_users (email) / target_roles / target_groups")
	case r.OperatorID != "":
		return newActionValidationError(ErrCodeActionRoutingDirectUser, "", fieldPath+".operator_id",
			"by-user-id routing is not portable across tenants; use target_users (email) / target_roles / target_groups")
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
		// Hybrid: the integration produces an ExternalSystemRecord, whose
		// external_system has no parent to default from here — require it.
		return validateIntegration(a, kg.Integration, fp+".integration", true)
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
	// external_system_record's external_system defaults from ext.System, so the
	// integration.system is optional here.
	return validateIntegration(a, ext.Integration, fp+".integration", false)
}

// validateIntegration enforces that an integration declares a tool and maps
// external_record_id from the tool result (the searchable correlation key).
// requireSystem is true for a hybrid knowledge_graph_entity integration, where
// integration.system is the only source for the ExternalSystemRecord's
// external_system column; false for external_system_record (it defaults from
// the parent ExternalSystemRecordDef.System).
func validateIntegration(a *ActionDef, integ *IntegrationDef, fieldPath string, requireSystem bool) error {
	if integ.Tool == "" {
		return newActionValidationError(ErrCodeActionExecutorRequired, a.Name, fieldPath+".tool", "required")
	}
	if requireSystem && integ.System == "" {
		return newActionValidationError(ErrCodeActionExternalSystem, a.Name, fieldPath+".system",
			"required for a knowledge_graph_entity integration (names the external system the outcome is mirrored to)")
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

// validateRelationships checks each relationship's source prefix, direction,
// and edge declaration. The edge's STRUCTURE is validated here (exactly one of
// edge_type|edge; long-form edge type/name/schema); whether the edge type
// resolves in / registers into the active ontology is a platform install-time
// concern (OGA-DKIT-VAL-1043), not an SDK structural check.
func validateRelationships(a *ActionDef, rels []RelDef) error {
	for j := range rels {
		r := rels[j]
		field := fmt.Sprintf("relationships[%d]", j)
		if !strings.HasPrefix(r.Source, "event.") && !strings.HasPrefix(r.Source, "payload.") {
			return newActionValidationError(ErrCodeActionRelSource, a.Name, field+".source",
				fmt.Sprintf("must start with 'event.' or 'payload.', got %q", r.Source))
		}
		if r.Direction != relDirectionOutgoing && r.Direction != relDirectionIncoming {
			return newActionValidationError(ErrCodeActionRelDirection, a.Name, field+".direction",
				fmt.Sprintf("must be outgoing|incoming, got %q", r.Direction))
		}
		// Exactly one of the short form (edge_type) or long form (edge) must be set.
		hasShort := r.EdgeType != ""
		hasLong := r.Edge != nil
		if hasShort == hasLong {
			return newActionValidationError(ErrCodeActionRelEdge, a.Name, field,
				"must set exactly one of edge_type (short form) | edge (long form)")
		}
		if hasLong {
			if err := validateEdgeDef(a, r.Edge, field+".edge"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateEdgeDef validates the long-form edge declaration's structure:
// type ∈ {existing,new}, name required, schema required+valid for new (valid if
// present otherwise). Edge-type existence/registration in the ontology is a
// platform install-time concern, not validated here.
func validateEdgeDef(a *ActionDef, e *EdgeDef, fp string) error {
	switch e.Type {
	case EntityTypeExisting, EntityTypeNew:
	default:
		return newActionValidationError(ErrCodeActionEdgeType, a.Name, fp+".type",
			fmt.Sprintf("must be existing|new, got %q", e.Type))
	}
	if e.Name == "" {
		return newActionValidationError(ErrCodeActionEdgeName, a.Name, fp+".name", "required")
	}
	if e.Type == EntityTypeNew && len(e.Schema) == 0 {
		return newActionValidationError(ErrCodeActionSchemaRequired, a.Name, fp+".schema",
			"required when edge.type=new")
	}
	if len(e.Schema) > 0 {
		if err := compileActionSchema(e.Schema); err != nil {
			return newActionValidationError(ErrCodeActionSchemaInvalid, a.Name, fp+".schema",
				fmt.Sprintf("not a valid JSON Schema 2020-12: %v", err))
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
