package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Entity type and direction constants for action declarations.
const (
	EntityTypeExisting          = "existing"
	EntityTypeNew               = "new"
	EntityTypeExternalReference = "external_reference"

	relDirectionOutgoing = "outgoing"
	relDirectionIncoming = "incoming"
)

// validateActions runs the full action-declaration validation pass over a
// loaded profile, returning the first ActionValidationError encountered. A
// profile with no proactive actions passes trivially.
func validateActions(p *DomainAgentProfile) error {
	for i := range p.Actions() {
		a := &p.ProactiveReasoning.Actions[i]
		if err := validateAction(a); err != nil {
			return err
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

	switch a.Entity.Type {
	case EntityTypeExisting, EntityTypeNew, EntityTypeExternalReference:
	default:
		return newActionValidationError(ErrCodeActionEntityType, a.Name, "entity.type",
			fmt.Sprintf("must be existing|new|external_reference, got %q", a.Entity.Type))
	}

	needsSchema := a.Entity.Type == EntityTypeNew || a.Entity.Type == EntityTypeExternalReference
	if needsSchema && len(a.Entity.Schema) == 0 {
		return newActionValidationError(ErrCodeActionSchemaRequired, a.Name, "entity.schema",
			fmt.Sprintf("required when entity.type=%s", a.Entity.Type))
	}
	if len(a.Entity.Schema) > 0 {
		if err := compileActionSchema(a.Entity.Schema); err != nil {
			return newActionValidationError(ErrCodeActionSchemaInvalid, a.Name, "entity.schema",
				fmt.Sprintf("not a valid JSON Schema 2020-12: %v", err))
		}
	}

	if a.Entity.Type == EntityTypeExternalReference {
		if a.Entity.ExternalSystem == "" {
			return newActionValidationError(ErrCodeActionExternalSystem, a.Name, "entity.external_system",
				"required when entity.type=external_reference")
		}
		if a.Executor == nil {
			return newActionValidationError(ErrCodeActionExecutorRequired, a.Name, "executor",
				"required when entity.type=external_reference")
		}
	}

	for j := range a.Relationships {
		r := a.Relationships[j]
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

	if a.AutoApproveTimeout != "" {
		if _, err := time.ParseDuration(a.AutoApproveTimeout); err != nil {
			return newActionValidationError(ErrCodeActionAutoApprove, a.Name, "auto_approve_timeout",
				fmt.Sprintf("not a valid Go duration: %v", err))
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
