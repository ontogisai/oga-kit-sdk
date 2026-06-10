package gateway

import "time"

// This file holds the canonical action-approval (HITL) types and enums shared
// across the platform. They live in the SDK so there is exactly one definition:
// the platform's `workflows` package consumes them via Go type aliases
// (`type ActionProposal = gateway.ActionProposal`, etc.), which makes drift
// between the kit-facing contract and the platform-internal contract
// structurally impossible.
//
// The SDK uses domain-agnostic terminology (TargetUserID, not OperatorID) so
// any vertical kit can consume the contract without mental translation.
//
// Spec: .kiro/specs/proactive-action-handling/design.md — "SDK-canonical types".

// HumanActionMode is the static governance property declared per action by the
// kit author. It decides whether an action executes after approval or is purely
// advisory. It is NEVER an LLM output — the reasoning model influences the mode
// only by choosing which action to propose.
type HumanActionMode string

const (
	// HumanActionModeApproval — operator Approve/Reject; the action executes on approval.
	HumanActionModeApproval HumanActionMode = "approval"
	// HumanActionModeAcknowledgement — operator Acknowledge/Dismiss; nothing executes.
	HumanActionModeAcknowledgement HumanActionMode = "acknowledgement"
)

// IsValid reports whether m is a recognized human-action mode.
func (m HumanActionMode) IsValid() bool {
	return m == HumanActionModeApproval || m == HumanActionModeAcknowledgement
}

// RiskLevel classifies the risk of a proposed action. Declared per action by
// the kit author (the legacy spec.behavior.risk_classification map is retired).
type RiskLevel string

const (
	RiskLevelInformational RiskLevel = "informational"
	RiskLevelLow           RiskLevel = "low"
	RiskLevelMedium        RiskLevel = "medium"
	RiskLevelHigh          RiskLevel = "high"
)

// IsValid reports whether r is one of the four canonical risk levels.
func (r RiskLevel) IsValid() bool {
	switch r {
	case RiskLevelInformational, RiskLevelLow, RiskLevelMedium, RiskLevelHigh:
		return true
	default:
		return false
	}
}

// ApprovalAction represents the operator's decision on a proposed action.
// The "modified" action was dropped per design principle 7 — operators who
// want changes Investigate, discuss with Frontier, and Frontier submits a new
// proposal that supersedes the original.
type ApprovalAction string

const (
	ApprovalActionApproved     ApprovalAction = "approved"
	ApprovalActionRejected     ApprovalAction = "rejected"
	ApprovalActionAcknowledged ApprovalAction = "acknowledged"
	ApprovalActionDismissed    ApprovalAction = "dismissed"
	ApprovalActionEscalated    ApprovalAction = "escalated"
	ApprovalActionSuperseded   ApprovalAction = "superseded"
)

// ApprovalStatus is the terminal status of an approval workflow.
type ApprovalStatus string

const (
	ApprovalStatusAutoApproved ApprovalStatus = "auto_approved"
	ApprovalStatusApproved     ApprovalStatus = "approved"
	ApprovalStatusRejected     ApprovalStatus = "rejected"
	ApprovalStatusAcknowledged ApprovalStatus = "acknowledged"
	ApprovalStatusDismissed    ApprovalStatus = "dismissed"
	ApprovalStatusEscalated    ApprovalStatus = "escalated"
	ApprovalStatusSuperseded   ApprovalStatus = "superseded"
	ApprovalStatusExpired      ApprovalStatus = "expired"
	ApprovalStatusFailed       ApprovalStatus = "failed"
)

// EventType identifies a canonical NATS event payload.
type EventType string

const (
	// EventTypeActionProposed is carried on agent.{tenant}.approval.required.
	EventTypeActionProposed EventType = "agent.action.proposed"
	// EventTypeActionResolved is carried on agent.{tenant}.approval.resolved.
	EventTypeActionResolved EventType = "agent.action.resolved"
)

// ActionRouting describes recipient targeting and channel preferences. Used
// both for primary delivery (ActionProposal.Routing) and for escalation when
// EscalationTimeout fires (ActionProposal.EscalationRouting).
//
// At least one of TargetUserID / TargetRoles / TargetGroups must be populated.
type ActionRouting struct {
	// TargetUserID addresses a specific user (operator OR supervisor) by id.
	// When set, TargetRoles and TargetGroups are ignored.
	TargetUserID string `json:"target_user_id,omitempty"`

	// TargetRoles lists platform roles whose members should receive the
	// notification (e.g., ["fm_manager"]).
	TargetRoles []string `json:"target_roles,omitempty"`

	// TargetGroups lists operator groups (e.g., "fm-managers-night-shift").
	TargetGroups []string `json:"target_groups,omitempty"`

	// Channels constrains delivery channels: nil/[] honors recipient
	// preferences; ["all"] broadcasts; explicit list forces those channels.
	Channels []string `json:"channels,omitempty"`
}

// HasTarget reports whether at least one recipient target is populated.
func (r ActionRouting) HasTarget() bool {
	return r.TargetUserID != "" || len(r.TargetRoles) > 0 || len(r.TargetGroups) > 0
}

// ActionProposal is the kit-author input contract for a proposed action. The
// kit author supplies the action data + reasoning + routing intent; the SDK
// packs the profile-derived governance fields (HumanActionMode, RiskLevel,
// AutoApprove*, Escalation*, NotificationHoldWindow) from the chosen action
// declaration; the gateway and workflow add the remaining fields downstream
// (see ActionProposedEvent).
type ActionProposal struct {
	// --- Kit-author supplied (via SubmitActionInput) ---

	ProposalID      string         `json:"proposal_id"`    // SDK generates a UUID if empty
	ActionType      string         `json:"action_type"`    // matches profile actions[*].name
	ActionPayload   map[string]any `json:"action_payload"` // validated against action.entity.schema
	Description     string         `json:"description"`    // 1-2 sentence operator summary
	Reasoning       string         `json:"reasoning"`      // full chain of thought
	ReasoningFacts  []string       `json:"reasoning_facts,omitempty"`
	ExpectedOutcome string         `json:"expected_outcome"`
	Routing         ActionRouting  `json:"routing"` // primary delivery intent
	TriggerEventID  string         `json:"trigger_event_id,omitempty"`
	ProposedAt      time.Time      `json:"proposed_at"`

	// --- SDK packs these from the loaded profile (chosen action + escalation policy) ---

	HumanActionMode        HumanActionMode `json:"human_action_mode"`
	RiskLevel              RiskLevel       `json:"risk_level"`
	AutoApproveTimeout     time.Duration   `json:"auto_approve_timeout,omitempty"`
	AutoApproveEligible    bool            `json:"auto_approve_eligible"`
	EscalationTimeout      time.Duration   `json:"escalation_timeout,omitempty"`
	NotificationHoldWindow time.Duration   `json:"notification_hold_window,omitempty"`

	// EscalationRouting carries the routing intent when no operator responds
	// within EscalationTimeout. Same resolution semantics as Routing.
	EscalationRouting ActionRouting `json:"escalation_routing,omitempty"`
}

// ActionProposedEvent is the canonical NATS event payload published on
// agent.{tenant}.approval.required. It composes ActionProposal (Go embedding —
// the JSON serializes flat) plus platform-derived fields. Kit authors subscribe
// to it; they never construct it.
type ActionProposedEvent struct {
	ActionProposal

	// Set by the gateway from authenticated request context.
	EventType EventType `json:"event_type"` // EventTypeActionProposed
	TenantID  string    `json:"tenant_id"`

	// AgentRegistrationID is the unique discovery ID of the proposing agent
	// (kit sidecar or personalised-agent instance). Always references an
	// AgentRegistration vertex.
	AgentRegistrationID string `json:"agent_registration_id"`

	// CurrentTrustLevel is the proposing agent's trust score at proposal time.
	CurrentTrustLevel float64 `json:"current_trust_level"`

	// Set by the workflow at NotifyOperator publish time. RiskLevel is NOT
	// here — it is inherited from the embedded ActionProposal.
	WorkflowID           string                       `json:"workflow_id"`
	ExpiresAt            time.Time                    `json:"expires_at"`
	InvestigationContext *InvestigationContextPayload `json:"investigation_context,omitempty"`
	CurrentApprovalLevel int                          `json:"current_approval_level"`
	TotalApprovalLevels  int                          `json:"total_approval_levels"`
	Timestamp            time.Time                    `json:"timestamp"`
}

// InvestigationContextPayload is the serializable investigation context
// included in notification payloads. Channels render an [Investigate] button
// only when this is populated; tapping it opens an investigation session keyed
// by these fields.
type InvestigationContextPayload struct {
	ProposalID     string   `json:"proposal_id"`
	WorkflowID     string   `json:"workflow_id"`
	AgentID        string   `json:"agent_id"`
	AgentType      string   `json:"agent_type"`
	TenantID       string   `json:"tenant_id"`
	ReasoningFacts []string `json:"reasoning_facts,omitempty"`
}

// ApprovalDecision is the signal payload an operator (or the auto-approve timer)
// produces for a pending approval workflow.
type ApprovalDecision struct {
	// Action is the operator's decision.
	Action ApprovalAction `json:"action"`

	// Reason explains the decision (required for rejection).
	Reason string `json:"reason,omitempty"`

	// DecidedBy identifies who made the decision ("auto:timer" for auto-approve).
	DecidedBy string `json:"decided_by"`

	// SourceChannel records the channel the operator used ("auto" for timer).
	SourceChannel string `json:"source_channel,omitempty"`

	// DecidedAt is when the decision was made.
	DecidedAt time.Time `json:"decided_at"`
}

// ApprovalResolvedEvent is the canonical NATS event payload published on
// agent.{tenant}.approval.resolved when a workflow reaches a terminal state.
type ApprovalResolvedEvent struct {
	EventType  EventType      `json:"event_type"` // EventTypeActionResolved
	TenantID   string         `json:"tenant_id"`
	ProposalID string         `json:"proposal_id"`
	WorkflowID string         `json:"workflow_id"`
	Status     ApprovalStatus `json:"status"`

	// DecidedBy / SourceChannel describe who resolved it and how. Empty for
	// expired / superseded / failed (no operator decision).
	DecidedBy     string `json:"decided_by,omitempty"`
	SourceChannel string `json:"source_channel,omitempty"`

	// Reason carries operator rationale OR (Status=failed) the error message.
	Reason string `json:"reason,omitempty"`

	// ExecutionStatus is "executed" | "skipped" | "failed" | "not_applicable".
	ExecutionStatus string `json:"execution_status,omitempty"`

	// OutcomeEntityID is the KG vertex created by execution (when applicable).
	OutcomeEntityID string `json:"outcome_entity_id,omitempty"`

	CompletedAt time.Time `json:"completed_at"`
}

// SubmitActionInput is the high-level kit-facing input to SubmitAction. It
// carries the kit-author fields directly plus the profile-derived governance
// fields. When the SDK's default proactive handler builds this, it populates
// the governance fields from the chosen action declaration; a kit author using
// a custom handler reads them off the loaded profile the same way.
type SubmitActionInput struct {
	// --- Kit-author fields ---
	ActionName      string         // matches profile actions[*].name → ActionProposal.ActionType
	Payload         map[string]any // validated against entity.schema before submit
	Description     string
	Reasoning       string
	ReasoningFacts  []string
	ExpectedOutcome string
	Routing         ActionRouting // at least one target field required
	TriggerEventID  string

	// --- Profile-derived governance fields (packed by the SDK from the chosen action) ---
	HumanActionMode        HumanActionMode
	RiskLevel              RiskLevel
	AutoApproveTimeout     time.Duration
	AutoApproveEligible    bool
	EscalationTimeout      time.Duration
	EscalationRouting      ActionRouting
	NotificationHoldWindow time.Duration
}

// ActionProposalSubmission is the result of submitting an action proposal.
type ActionProposalSubmission struct {
	WorkflowID string `json:"workflow_id"`
	ProposalID string `json:"proposal_id"` // echoed for confirmation
}
