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

	// Entity describes the KG entity (or external reference) the action produces.
	Entity EntityDef `yaml:"entity"`

	// Executor optionally routes execution through an MCP tool.
	Executor *ExecutorDef `yaml:"executor,omitempty"`

	// Relationships declares the edges created from the produced entity.
	Relationships []RelDef `yaml:"relationships,omitempty"`

	// Triggers is an OPTIONAL coarse candidate-narrowing hint, NOT a selector.
	// When present, it filters the candidate catalog offered to the reasoning
	// LLM. When absent, the action is always a candidate. The reasoning LLM
	// always makes the final selection.
	Triggers []TriggerDef `yaml:"triggers,omitempty"`
}

// EntityDef describes the entity an action produces.
type EntityDef struct {
	// Type is "existing" | "new" | "external_reference".
	Type string `yaml:"type"`

	// Name is the entity/relationship type name (for existing/new).
	Name string `yaml:"name,omitempty"`

	// ExternalSystem identifies the target system (for external_reference).
	ExternalSystem string `yaml:"external_system,omitempty"`

	// Schema is a JSON Schema 2020-12 document. Required for new /
	// external_reference; optional override for existing.
	Schema map[string]any `yaml:"schema,omitempty"`
}

// ExecutorDef routes action execution through an MCP tool.
type ExecutorDef struct {
	Tool          string         `yaml:"tool"`
	ResultMapping map[string]any `yaml:"result_mapping,omitempty"`
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
// gateway.ActionRouting one-for-one but carries yaml tags (gateway.ActionRouting
// has json tags only). The proactive handler converts it to gateway.ActionRouting
// at submit time via ToActionRouting. At least one of TargetUserID / TargetRoles
// / TargetGroups must be set.
type RoutingDef struct {
	// TargetUserID addresses a specific user by id. When set, the role and
	// group targets are ignored by the notification-router.
	TargetUserID string `yaml:"target_user_id,omitempty"`

	// TargetRoles lists platform roles whose members receive the notification
	// (e.g. ["fm_operator"]). The most common form for kit-declared routing.
	TargetRoles []string `yaml:"target_roles,omitempty"`

	// TargetGroups lists operator groups (e.g. ["fm-managers-night-shift"]).
	TargetGroups []string `yaml:"target_groups,omitempty"`

	// Channels constrains delivery channels: empty honors each recipient's
	// preferences; ["all"] broadcasts; an explicit list forces those channels.
	Channels []string `yaml:"channels,omitempty"`
}

// HasTarget reports whether at least one recipient target is populated.
func (r *RoutingDef) HasTarget() bool {
	if r == nil {
		return false
	}
	return r.TargetUserID != "" || len(r.TargetRoles) > 0 || len(r.TargetGroups) > 0
}

// ToActionRouting converts the YAML routing form to the canonical gateway type.
func (r *RoutingDef) ToActionRouting() gateway.ActionRouting {
	if r == nil {
		return gateway.ActionRouting{}
	}
	return gateway.ActionRouting{
		TargetUserID: r.TargetUserID,
		TargetRoles:  r.TargetRoles,
		TargetGroups: r.TargetGroups,
		Channels:     r.Channels,
	}
}

// EscalationPolicyDef declares how a proposal escalates when no operator decides
// within Timeout. All fields are optional; an empty policy means "no escalation
// routing, rely on platform defaults".
type EscalationPolicyDef struct {
	// Timeout is the Go-duration string after which an undecided proposal
	// escalates (e.g. "30m"). Parsed + validated at load (OGA-DKIT-VAL-1041).
	Timeout string `yaml:"timeout,omitempty"`

	// NotificationHoldWindow delays operator notification by this duration so a
	// convergence agent can supersede the proposal first (e.g. "5s"). Parsed +
	// validated at load (OGA-DKIT-VAL-1041).
	NotificationHoldWindow string `yaml:"notification_hold_window,omitempty"`

	// Routing is the escalation recipient — same shape as the primary routing.
	Routing RoutingDef `yaml:"routing,omitempty"`
}
