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
	ErrCodeActionEntityType       = "OGA-DKIT-VAL-1032" // entity.type invalid
	ErrCodeActionSchemaRequired   = "OGA-DKIT-VAL-1033" // entity.schema required (new/external_reference)
	ErrCodeActionSchemaInvalid    = "OGA-DKIT-VAL-1034" // entity.schema not valid JSON Schema 2020-12
	ErrCodeActionExternalSystem   = "OGA-DKIT-VAL-1035" // external_system required (external_reference)
	ErrCodeActionExecutorRequired = "OGA-DKIT-VAL-1036" // executor required (external_reference)
	ErrCodeActionRelSource        = "OGA-DKIT-VAL-1037" // relationships[].source bad prefix
	ErrCodeActionRelDirection     = "OGA-DKIT-VAL-1038" // relationships[].direction invalid
	ErrCodeActionAutoApprove      = "OGA-DKIT-VAL-1039" // auto_approve_timeout unparseable
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
