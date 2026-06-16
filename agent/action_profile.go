package agent

import (
	"time"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// This file defines the kit-facing action-schema structs that hang off
// ProactiveConfig.Actions, plus the profile helper methods the proactive
// handler uses to select candidate actions.
//
// human_action_mode and risk_level are parsed as plain strings here (not the
// gateway.HumanActionMode / gateway.RiskLevel enums) deliberately: it keeps the
// agent package from importing gateway, and matches idiomatic YAML decoding.
// The proactive handler converts the validated strings to the typed enums when
// it builds the gateway.SubmitActionInput. Validation happens at load time
// (see action_validation.go).
//
// Spec: .kiro/specs/proactive-action-handling/design.md — "Action Schema".

// ActionDef declares one proactive action an agent may propose.
type ActionDef struct {
	// Name is the snake_case action identifier, unique within the profile.
	// Matches ActionProposal.ActionType.
	Name string `yaml:"name"`

	// Description is the operator-facing summary of what the action does.
	Description string `yaml:"description"`

	// HumanActionMode is "approval" or "acknowledgement" — a static governance
	// property, never an LLM output.
	HumanActionMode string `yaml:"human_action_mode"`

	// RiskLevel is "informational" | "low" | "medium" | "high".
	RiskLevel string `yaml:"risk_level"`

	// AutoApproveTimeout optionally overrides the per-risk-level default.
	// Parsed as a Go duration string (e.g. "5m", "0") and validated at load.
	AutoApproveTimeout string `yaml:"auto_approve_timeout,omitempty"`

	// Outcome declares what the action produces — exactly one of the two
	// outcome intents (knowledge_graph_entity | external_system_record).
	Outcome OutcomeDef `yaml:"outcome"`

	// Triggers is an OPTIONAL coarse candidate-narrowing hint, NOT a selector.
	// When present, it filters the candidate catalog offered to the reasoning
	// LLM. When absent, the action is always a candidate. The reasoning LLM
	// always makes the final selection.
	Triggers []TriggerDef `yaml:"triggers,omitempty"`
}

// OutcomeDef declares what an action produces. Exactly one of the two intents
// must be set (validated at load):
//
//   - knowledge_graph_entity — a first-class domain entity in the Knowledge
//     Graph (the platform writes it). May optionally also sync to an external
//     system via an integration sub-block (the hybrid pattern).
//   - external_system_record — the record lives only in an external system; the
//     KG keeps a lightweight reference vertex (ExternalSystemRecord). Requires
//     an integration.
//
// "external" never means "not in the graph": an external_system_record still
// produces an ExternalSystemRecord vertex — the distinction is domain entity vs
// platform reference vertex, and which system owns the record.
type OutcomeDef struct {
	KnowledgeGraphEntity *KnowledgeGraphEntityDef `yaml:"knowledge_graph_entity,omitempty"`
	ExternalSystemRecord *ExternalSystemRecordDef `yaml:"external_system_record,omitempty"`
}

// KnowledgeGraphEntityDef describes a first-class KG domain entity outcome.
type KnowledgeGraphEntityDef struct {
	// Type is "existing" (a type already in the active ontology) or "new" (a
	// type the kit declares here, registered at install).
	Type string `yaml:"type"`

	// Name is the entity type name (tenant-prefixed at install).
	Name string `yaml:"name"`

	// Schema is a JSON Schema 2020-12 document. Required when type=new; an
	// optional narrowing override for type=existing (the platform lifts the
	// schema from the active ontology when omitted).
	Schema map[string]any `yaml:"schema,omitempty"`

	// Relationships declares the edges created from the produced entity.
	Relationships []RelDef `yaml:"relationships,omitempty"`

	// Integration optionally syncs the outcome to an external system as part of
	// producing it (the hybrid pattern). The platform still writes the KG
	// entity; the integration result populates an ExternalSystemRecord linked
	// by a MIRRORS edge.
	Integration *IntegrationDef `yaml:"integration,omitempty"`
}

// ExternalSystemRecordDef describes an outcome that lives only in an external
// system, recorded in the KG as an ExternalSystemRecord reference vertex.
type ExternalSystemRecordDef struct {
	// System identifies the target external system (e.g. "sap").
	System string `yaml:"system"`

	// Schema is a JSON Schema 2020-12 document describing the payload sent to
	// the integration tool. Required.
	Schema map[string]any `yaml:"schema,omitempty"`

	// Integration is the external-system call. Required for this mode.
	Integration *IntegrationDef `yaml:"integration,omitempty"`
}

// IntegrationDef routes execution through an MCP tool for custom processing
// and/or external-system integration. The platform never lets the tool write
// the KG entity — the platform owns KG writes; the tool result is recorded as
// an ExternalSystemRecord and/or mapped per ResultMapping.
type IntegrationDef struct {
	// System labels the external system the outcome is recorded in (drives the
	// ExternalSystemRecord.external_system column). REQUIRED for a hybrid
	// knowledge_graph_entity integration (it has no parent system to default
	// from); optional for an external_system_record, where it defaults to the
	// parent System when empty.
	System string `yaml:"system,omitempty"`

	// Tool is the MCP tool name invoked via the Platform Access Gateway. Required.
	Tool string `yaml:"tool"`

	// ResultMapping maps fields from the tool's JSON result onto the
	// ExternalSystemRecord columns: keys are the fixed columns, values are the
	// source field path in the tool result. `external_record_id` is REQUIRED
	// when an integration is present. `status` is optional. Any other key is
	// promoted onto the ExternalSystemRecord as an un-indexed property (the full
	// response is always stored in ExternalSystemRecord.result_json).
	ResultMapping map[string]string `yaml:"result_mapping,omitempty"`
}

// RelDef declares an edge from the produced entity to another entity.
type RelDef struct {
	// Source is "event.X" or "payload.X".
	Source string `yaml:"source"`

	// EdgeType is the short form — implies an existing edge type.
	EdgeType string `yaml:"edge_type,omitempty"`

	// Edge is the long form for declaring a new edge type.
	Edge *EdgeDef `yaml:"edge,omitempty"`

	// Direction is "outgoing" or "incoming".
	Direction string `yaml:"direction"`
}

// EdgeDef is the long-form edge declaration.
type EdgeDef struct {
	Type   string         `yaml:"type"`
	Name   string         `yaml:"name"`
	Schema map[string]any `yaml:"schema,omitempty"`
}

// Outcome mode identifiers (the OutcomeDef discriminator).
const (
	OutcomeKnowledgeGraphEntity = "knowledge_graph_entity"
	OutcomeExternalSystemRecord = "external_system_record"
)

// Mode returns the populated outcome mode, or "" when neither (or both) is set.
func (o OutcomeDef) Mode() string {
	kg := o.KnowledgeGraphEntity != nil
	ext := o.ExternalSystemRecord != nil
	switch {
	case kg && !ext:
		return OutcomeKnowledgeGraphEntity
	case ext && !kg:
		return OutcomeExternalSystemRecord
	default:
		return ""
	}
}

// PayloadSchema returns the JSON Schema the LLM payload must conform to for this
// action — the knowledge_graph_entity / external_system_record schema. Nil when
// none is declared (e.g. type=existing without an override; the platform lifts
// the schema from the active ontology at execution time).
func (a *ActionDef) PayloadSchema() map[string]any {
	switch {
	case a.Outcome.KnowledgeGraphEntity != nil:
		return a.Outcome.KnowledgeGraphEntity.Schema
	case a.Outcome.ExternalSystemRecord != nil:
		return a.Outcome.ExternalSystemRecord.Schema
	default:
		return nil
	}
}

// Integration returns the action's integration block (from whichever outcome
// mode is set), or nil when the action has no integration.
func (a *ActionDef) Integration() *IntegrationDef {
	switch {
	case a.Outcome.KnowledgeGraphEntity != nil:
		return a.Outcome.KnowledgeGraphEntity.Integration
	case a.Outcome.ExternalSystemRecord != nil:
		return a.Outcome.ExternalSystemRecord.Integration
	default:
		return nil
	}
}

// TriggerDef is a coarse candidate-narrowing hint for an action.
type TriggerDef struct {
	// EventType narrows candidacy to events of this type (e.g. "EntityAnomalyEvent").
	EventType string `yaml:"event_type"`

	// Condition is a CEL/JSONPath match hint. Full evaluation is a follow-up;
	// today an action whose trigger EventType matches is a candidate regardless
	// of Condition.
	Condition string `yaml:"condition,omitempty"`
}

// ProactiveEvent is the parsed representation of an inbound proactive A2A
// message (metadata.intent == "proactive_event"). The proactive handler builds
// it from the A2A message and uses it to select candidate actions and seed the
// grounding strategy.
type ProactiveEvent struct {
	EventID    string         `json:"event_id,omitempty"`
	EventType  string         `json:"event_type"`
	EntityID   string         `json:"entity_id"`
	EntityType string         `json:"entity_type,omitempty"`
	TenantID   string         `json:"tenant_id"`
	H3Cell     string         `json:"h3_cell,omitempty"`
	Severity   string         `json:"severity,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Timestamp  time.Time      `json:"timestamp,omitempty"`
}

// Actions returns the declared action catalog (nil-safe).
func (p *DomainAgentProfile) Actions() []ActionDef {
	if p == nil || p.ProactiveReasoning == nil {
		return nil
	}
	return p.ProactiveReasoning.Actions
}

// Action returns the declared action with the given name.
func (p *DomainAgentProfile) Action(name string) (*ActionDef, bool) {
	for i := range p.Actions() {
		if p.ProactiveReasoning.Actions[i].Name == name {
			return &p.ProactiveReasoning.Actions[i], true
		}
	}
	return nil, false
}

// CandidateActions returns the actions eligible to be proposed for the given
// event. An action with no triggers is always a candidate. An action with
// triggers is a candidate when any trigger's EventType matches the event's
// EventType (an empty trigger EventType matches any event). Triggers only
// NARROW the catalog — the reasoning LLM still makes the final selection.
func (p *DomainAgentProfile) CandidateActions(event *ProactiveEvent) []ActionDef {
	all := p.Actions()
	if len(all) == 0 {
		return nil
	}
	out := make([]ActionDef, 0, len(all))
	for _, a := range all {
		if len(a.Triggers) == 0 {
			out = append(out, a)
			continue
		}
		for _, t := range a.Triggers {
			if t.EventType == "" || (event != nil && t.EventType == event.EventType) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// RoutingDef is the kit-facing YAML form of a routing target. It mirrors
// gateway.ActionRouting but carries yaml tags (gateway.ActionRouting has json
// tags only). The proactive handler converts it to gateway.ActionRouting at
// submit time via ToActionRouting. At least one of TargetRoles / TargetGroups
// must be set.
//
// Kit-authored routing addresses recipients by ROLE or GROUP only. Addressing a
// specific user (by id or email) is non-portable across tenants and is rejected
// at manifest validation (OGA-DKIT-VAL-1042) — per-user routing is a
// tenant-override concern resolved at delivery time, never declared in a kit
// bundle.
type RoutingDef struct {
	// TargetRoles lists platform roles whose members receive the notification
	// (e.g. ["fm_operator"]). The most common form for kit-declared routing.
	TargetRoles []string `yaml:"target_roles,omitempty"`

	// TargetGroups lists user groups (e.g. ["fm-managers-night-shift"]).
	TargetGroups []string `yaml:"target_groups,omitempty"`

	// Channels constrains delivery channels: empty honors each recipient's
	// preferences; ["all"] broadcasts; an explicit list forces those channels.
	Channels []string `yaml:"channels,omitempty"`

	// NotificationHoldWindow delays delivery of the PRIMARY operator
	// notification by this Go-duration string (e.g. "5s") so a convergence
	// agent can correlate and supersede the proposal before the operator sees
	// it. Empty / unset = "0" = no hold. Parsed + validated at load
	// (OGA-DKIT-VAL-1042). Only meaningful on the primary proactive_reasoning.
	// routing — ignored on escalation_policy.routing.
	NotificationHoldWindow string `yaml:"notification_hold_window,omitempty"`

	// TargetUserID / UserID / OperatorID are REJECTED catch fields. Kit routing
	// must address recipients by target_roles / target_groups only — a user id
	// or email is non-portable across tenants. These fields exist solely so the
	// manifest loader produces the helpful OGA-DKIT-VAL-1050 error instead of a
	// generic "unknown field" decode error. Always rejected at load by
	// validateNoDirectUserRouting; never used for routing.
	TargetUserID string `yaml:"target_user_id,omitempty"`
	UserID       string `yaml:"user_id,omitempty"`
	OperatorID   string `yaml:"operator_id,omitempty"`
}

// HasTarget reports whether at least one recipient target is populated.
func (r *RoutingDef) HasTarget() bool {
	if r == nil {
		return false
	}
	return len(r.TargetRoles) > 0 || len(r.TargetGroups) > 0
}

// ToActionRouting converts the YAML routing form to the canonical gateway type.
// NotificationHoldWindow is parsed best-effort (already validated at load); an
// empty or unparseable value yields a zero hold (no delay).
func (r *RoutingDef) ToActionRouting() gateway.ActionRouting {
	if r == nil {
		return gateway.ActionRouting{}
	}
	var hold time.Duration
	if r.NotificationHoldWindow != "" {
		if d, err := time.ParseDuration(r.NotificationHoldWindow); err == nil {
			hold = d
		}
	}
	return gateway.ActionRouting{
		TargetRoles:            r.TargetRoles,
		TargetGroups:           r.TargetGroups,
		Channels:               r.Channels,
		NotificationHoldWindow: hold,
	}
}

// EscalationPolicyDef declares how a proposal escalates when no operator decides
// within Timeout. All fields are optional; an empty policy means "no escalation
// routing, rely on platform defaults". The notification hold window is NOT here
// — it is a primary-delivery concern and lives on proactive_reasoning.routing.
type EscalationPolicyDef struct {
	// Timeout is the Go-duration string after which an undecided proposal
	// escalates (e.g. "30m"). Parsed + validated at load (OGA-DKIT-VAL-1041).
	Timeout string `yaml:"timeout,omitempty"`

	// Routing is the escalation recipient — same shape as the primary routing.
	// Its NotificationHoldWindow (if set) is ignored: escalation has no
	// supersession window.
	Routing RoutingDef `yaml:"routing,omitempty"`
}
