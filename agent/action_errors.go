package agent

import "fmt"

// Action-schema validation error codes. These mirror the platform's
// docs/error-catalog.yaml DKIT-VAL range so a kit author who validates locally
// with LoadDomainAgentProfile gets the same code they would hit at install
// time under the platform's OGA-DKIT-VAL-1022 umbrella.
//
// Spec: .kiro/specs/proactive-action-handling/design.md — "Profile loader validation".
const (
	ErrCodeActionHumanMode        = "OGA-DKIT-VAL-1030" // human_action_mode invalid
	ErrCodeActionRiskLevel        = "OGA-DKIT-VAL-1031" // risk_level invalid
	ErrCodeActionEntityType       = "OGA-DKIT-VAL-1032" // knowledge_graph_entity.type invalid
	ErrCodeActionSchemaRequired   = "OGA-DKIT-VAL-1033" // schema required (kg type=new / external_system_record)
	ErrCodeActionSchemaInvalid    = "OGA-DKIT-VAL-1034" // schema not valid JSON Schema 2020-12
	ErrCodeActionExternalSystem   = "OGA-DKIT-VAL-1035" // external_system_record.system required
	ErrCodeActionExecutorRequired = "OGA-DKIT-VAL-1036" // integration required / integration.tool required
	ErrCodeActionRelSource        = "OGA-DKIT-VAL-1037" // relationships[].source bad prefix
	ErrCodeActionRelDirection     = "OGA-DKIT-VAL-1038" // relationships[].direction invalid
	ErrCodeActionAutoApprove      = "OGA-DKIT-VAL-1039" // auto_approve_timeout unparseable
	// 1040 is reserved (platform) for the event_subscriptions ontology
	// validation added in C4 — do NOT reuse it here.
	ErrCodeActionRoutingRequired = "OGA-DKIT-VAL-1041" // proactive_reasoning.routing required when actions declared
	ErrCodeActionEscalationDur   = "OGA-DKIT-VAL-1042" // routing/escalation duration unparseable
	// 1043 (action references undefined entity type) and 1044 (action executor
	// references unavailable tool) are platform-only install-time codes — the
	// SDK cannot resolve ontology types or the MCP catalog, so it never raises
	// them. Do NOT reuse them here.
	ErrCodeActionExternalRecordID = "OGA-DKIT-VAL-1045" // integration.result_mapping.external_record_id required
	ErrCodeActionOutcomeMode      = "OGA-DKIT-VAL-1046" // outcome must set exactly one of knowledge_graph_entity | external_system_record
	ErrCodeActionRelEdge          = "OGA-DKIT-VAL-1047" // relationships[].* must set exactly one of edge_type | edge
	ErrCodeActionEdgeType         = "OGA-DKIT-VAL-1048" // relationships[].edge.type invalid (must be existing|new)
	ErrCodeActionEdgeName         = "OGA-DKIT-VAL-1049" // relationships[].edge.name required
	// ErrCodeActionRoutingDirectUser rejects kit-authored routing that
	// addresses a recipient directly by user id (target_user_id, user_id,
	// operator_id). A user id is non-portable across tenants — kit routing must
	// address by target_users (email), target_roles, or target_groups. Emails
	// are portable distribution addresses and ARE allowed; only by-id is not.
	ErrCodeActionRoutingDirectUser = "OGA-DKIT-VAL-1050" // routing addresses a user by id (non-portable)
)

// ActionValidationError is the structured error returned when an action
// declaration in an agent profile fails validation at load time. It carries
// the catalog Code, the offending action Name, the Field path, and a message.
type ActionValidationError struct {
	Code    string
	Action  string
	Field   string
	Message string
}

// Error implements error.
func (e *ActionValidationError) Error() string {
	return fmt.Sprintf("%s: action %q field %q: %s", e.Code, e.Action, e.Field, e.Message)
}

// newActionValidationError is a small constructor for the validation pass.
func newActionValidationError(code, action, field, msg string) *ActionValidationError {
	return &ActionValidationError{Code: code, Action: action, Field: field, Message: msg}
}
