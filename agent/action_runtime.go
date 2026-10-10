package agent

import (
	"encoding/json"
	"errors"
	"time"
)

// IntentProactiveEvent is the A2A message metadata intent value that routes a
// message to the proactive handler.
const IntentProactiveEvent = "proactive_event"

// ActionNoOp is the sentinel ActionType the reasoning LLM emits when it
// concludes no action is warranted. The agent acks the event without submitting
// a proposal — a first-class outcome, not a failure.
const ActionNoOp = "no_action"

// ErrActionDecision indicates the reasoning LLM produced an action decision the
// runtime could not act on (e.g., an unknown action_type).
var ErrActionDecision = errors.New("invalid action decision")

// ActionDecision is the structured output of proactive reasoning. The LLM
// chooses ONE action from the candidate catalog (or ActionNoOp to decline) and
// produces the payload + reasoning. Payload conforms to the chosen action's
// outcome payload schema (the selected branch of the discriminated decision schema).
type ActionDecision struct {
	ActionType      string         `json:"action_type"`
	Payload         map[string]any `json:"payload,omitempty"`
	Description     string         `json:"description,omitempty"`
	Reasoning       string         `json:"reasoning"`
	ReasoningFacts  []string       `json:"reasoning_facts,omitempty"`
	ExpectedOutcome string         `json:"expected_outcome,omitempty"`
}

// readIntent extracts metadata["intent"] as a string (empty when absent).
func readIntent(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata["intent"].(string); ok {
		return v
	}
	return ""
}

// ParseProactiveEvent extracts a ProactiveEvent from an inbound A2A message.
// It first tries to JSON-decode the text part into a ProactiveEvent, then
// overlays any string fields present in the message metadata. When the body is
// not JSON, it builds the event from metadata alone. Requires event_type and
// entity_id to be resolvable.
func ParseProactiveEvent(msg *A2AMessage) (*ProactiveEvent, error) {
	if msg == nil || msg.Params == nil || msg.Params.Message == nil {
		return nil, errors.New("proactive event: message params required")
	}
	md := msg.Params.Message.Metadata
	event := &ProactiveEvent{}

	if text := ExtractText(msg.Params.Message.Parts); text != "" {
		// Best-effort JSON body; ignore parse errors and fall back to metadata.
		_ = json.Unmarshal([]byte(text), event)
	}
	overlayEventMetadata(event, md)

	if event.EventType == "" || event.EntityID == "" {
		return nil, errors.New("proactive event requires event_type and entity_id")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	return event, nil
}

// overlayEventMetadata applies non-empty string metadata fields onto the event.
func overlayEventMetadata(event *ProactiveEvent, md map[string]any) {
	if md == nil {
		return
	}
	set := func(key string, dst *string) {
		if v, ok := md[key].(string); ok && v != "" {
			*dst = v
		}
	}
	set("event_id", &event.EventID)
	set("event_type", &event.EventType)
	set("entity_id", &event.EntityID)
	set("entity_type", &event.EntityType)
	set("tenant_id", &event.TenantID)
	set("h3_cell", &event.H3Cell)
	set("severity", &event.Severity)
}

// AckNoProposal returns the agent response used when the agent reasons that no
// action is warranted (or no proactive handler is wired). It acknowledges the
// event without submitting a proposal.
func AckNoProposal(event *ProactiveEvent) *A2AResponse {
	msg := "no action proposed"
	if event != nil && event.EventType != "" {
		msg = "no action proposed for event " + event.EventType
	}
	return &A2AResponse{
		Message: &Message{
			Role:  "agent",
			Parts: []Part{{Text: msg}},
		},
	}
}

// AckAccepted returns the immediate acknowledgement the proactive handler sends
// back to the Event Router BEFORE running grounding + reasoning. The Event
// Router invokes proactive events over a synchronous A2A message/send bounded
// by a short client timeout and expects the agent to "acknowledge quickly and
// process async". Proactive reasoning runs on a detached context after this
// ack, so the router's timeout is a delivery-ack window, not a bound on the
// agent's reasoning time.
func AckAccepted(event *ProactiveEvent) *A2AResponse {
	msg := "proactive event accepted; processing asynchronously"
	if event != nil && event.EventType != "" {
		msg = "proactive event accepted; processing asynchronously: " + event.EventType
	}
	return &A2AResponse{
		Message: &Message{
			Role:  "agent",
			Parts: []Part{{Text: msg}},
		},
	}
}
