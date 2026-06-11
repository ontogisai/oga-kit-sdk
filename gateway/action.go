package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// WorkflowTypeAgentApproval is the platform workflow type that handles the
// HITL approval flow. SubmitActionProposal posts to /workflow with this type.
const WorkflowTypeAgentApproval = "agent-approval"

// ErrInvalidActionInput is returned by SubmitAction when the input fails
// client-side validation before the gateway round-trip.
var ErrInvalidActionInput = errors.New("invalid action submission input")

// SubmitAction is the high-level kit-facing API. It assembles an ActionProposal
// from the supplied input (kit-author fields + profile-derived governance
// fields), generates a ProposalID + ProposedAt when absent, and submits it via
// SubmitActionProposal. The SDK's default proactive handler and most custom
// kit handlers call this rather than the low-level method.
func (c *PlatformGatewayClient) SubmitAction(
	ctx context.Context, input *SubmitActionInput,
) (*ActionProposalSubmission, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: input is nil", ErrInvalidActionInput)
	}
	if input.ActionName == "" {
		return nil, fmt.Errorf("%w: action_name is required", ErrInvalidActionInput)
	}
	if !input.HumanActionMode.IsValid() {
		return nil, fmt.Errorf("%w: human_action_mode %q", ErrInvalidActionInput, input.HumanActionMode)
	}
	if !input.RiskLevel.IsValid() {
		return nil, fmt.Errorf("%w: risk_level %q", ErrInvalidActionInput, input.RiskLevel)
	}
	if !input.Routing.HasTarget() {
		return nil, fmt.Errorf("%w: routing requires at least one of target_user_id/target_roles/target_groups", ErrInvalidActionInput)
	}

	proposal := &ActionProposal{
		ActionType:          input.ActionName,
		ActionPayload:       input.Payload,
		Description:         input.Description,
		Reasoning:           input.Reasoning,
		ReasoningFacts:      input.ReasoningFacts,
		ExpectedOutcome:     input.ExpectedOutcome,
		Routing:             input.Routing,
		TriggerEventID:      input.TriggerEventID,
		TriggerEntityID:     input.TriggerEntityID,
		HumanActionMode:     input.HumanActionMode,
		RiskLevel:           input.RiskLevel,
		AutoApproveTimeout:  input.AutoApproveTimeout,
		AutoApproveEligible: input.AutoApproveEligible,
		EscalationTimeout:   input.EscalationTimeout,
		EscalationRouting:   input.EscalationRouting,
	}
	return c.SubmitActionProposal(ctx, proposal)
}

// SubmitActionProposal is the low-level wire API. It posts a fully-assembled
// ActionProposal to /workflow (type=agent-approval) and returns the workflow +
// proposal IDs. A ProposalID (UUID) and ProposedAt are generated when empty.
// The gateway enriches the body with TenantID / AgentRegistrationID /
// CurrentTrustLevel from the authenticated JWT before dispatching to Temporal.
func (c *PlatformGatewayClient) SubmitActionProposal(
	ctx context.Context, proposal *ActionProposal,
) (*ActionProposalSubmission, error) {
	if proposal == nil {
		return nil, fmt.Errorf("%w: proposal is nil", ErrInvalidActionInput)
	}
	if proposal.ProposalID == "" {
		proposal.ProposalID = uuid.NewString()
	}
	if proposal.ProposedAt.IsZero() {
		proposal.ProposedAt = time.Now().UTC()
	}

	body := map[string]any{
		"type":  WorkflowTypeAgentApproval,
		"input": proposal,
	}
	respData, err := c.post(ctx, "/workflow", body)
	if err != nil {
		return nil, fmt.Errorf("submit action proposal: %w", err)
	}

	var result struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, fmt.Errorf("parse action proposal response: %w", err)
	}
	return &ActionProposalSubmission{
		WorkflowID: result.WorkflowID,
		ProposalID: proposal.ProposalID,
	}, nil
}
