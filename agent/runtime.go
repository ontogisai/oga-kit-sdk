package agent

import (
	"context"
	"encoding/json"
)

// AgentRuntime defines the A2A contract that all domain agents must satisfy.
// The platform routes requests to agents via this protocol.
type AgentRuntime interface {
	// ServeAgentCard returns the agent's A2A card (served at
	// GET /.well-known/agent-card.json).
	ServeAgentCard() *AgentCard

	// HandleMessage processes a synchronous A2A message/send request.
	HandleMessage(ctx context.Context, msg *A2AMessage) (*A2AResponse, error)

	// HandleStream processes a streaming A2A message/stream request.
	// Events are written to the StreamWriter as they become available.
	HandleStream(ctx context.Context, msg *A2AMessage, stream StreamWriter) error

	// Healthz returns nil if the agent is alive (liveness probe).
	Healthz(ctx context.Context) error

	// Readyz returns nil if the agent is ready to serve traffic (readiness probe).
	Readyz(ctx context.Context) error
}

// StreamWriter is used by HandleStream to send SSE events to the client.
//
// The event argument is the typed StreamEvent envelope defined in events.go.
// httpStreamWriter (in serve.go) marshals it to SSE wire format; in-process
// consumers (Frontier mux.AddSource) consume the typed struct directly.
type StreamWriter interface {
	// WriteEvent sends a single SSE event to the client.
	WriteEvent(ctx context.Context, event *StreamEvent) error

	// Close signals the end of the stream.
	Close() error
}

// AgentCard is the A2A agent card served at /.well-known/agent-card.json.
type AgentCard struct {
	Name                string               `json:"name"`
	Description         string               `json:"description"`
	URL                 string               `json:"url"`
	Version             string               `json:"version"`
	SupportedInterfaces []SupportedInterface `json:"supportedInterfaces"`
	Capabilities        map[string]any       `json:"capabilities"`
	DefaultInputModes   []string             `json:"defaultInputModes"`
	DefaultOutputModes  []string             `json:"defaultOutputModes"`
	Skills              []Skill              `json:"skills"`
	Provider            *Provider            `json:"provider,omitempty"`
}

// SupportedInterface declares a protocol binding.
type SupportedInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

// Skill describes an agent capability.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

// Provider identifies the agent provider.
type Provider struct {
	Organization string `json:"organization"`
	URL          string `json:"url,omitempty"`
}

// A2AMessage is the inbound JSON-RPC 2.0 message from the A2A protocol.
type A2AMessage struct {
	// ID is the JSON-RPC request ID (can be string or number).
	ID json.RawMessage `json:"id,omitempty"`

	// Method is the JSON-RPC method (e.g., "message/send", "message/stream").
	Method string `json:"method"`

	// Params contains the message parameters.
	Params *MessageParams `json:"params,omitempty"`
}

// MessageParams contains the parameters of an A2A message.
type MessageParams struct {
	// Message is the user's message.
	Message *Message `json:"message,omitempty"`
}

// Message represents a single message in the A2A protocol.
type Message struct {
	// Role is "user" or "agent".
	Role string `json:"role"`

	// Parts are the message content parts.
	Parts []Part `json:"parts"`

	// Metadata carries A2A message metadata. The proactive dispatch reads
	// metadata["intent"] (== "proactive_event") to route the message to the
	// proactive handler. Event Router populates this for proactive events.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Part is a content part within a message.
type Part struct {
	// Text is the text content (non-empty for text parts).
	Text string `json:"text,omitempty"`

	// Data is structured data content (for non-text parts).
	Data json.RawMessage `json:"data,omitempty"`

	// MimeType is the MIME type for data parts.
	MimeType string `json:"mimeType,omitempty"`
}

// A2AResponse is the outbound JSON-RPC 2.0 response.
type A2AResponse struct {
	// Message is the agent's response message (simple response).
	Message *Message `json:"message,omitempty"`

	// Task is a task-based response (for tracked operations).
	Task *Task `json:"task,omitempty"`
}

// Task represents a tracked operation in the A2A protocol.
type Task struct {
	ID        string     `json:"id"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// TaskStatus represents the state of a task.
type TaskStatus struct {
	State   string `json:"state"` // "completed", "running", "failed"
	Message string `json:"message,omitempty"`
}

// Artifact is a result artifact from a task.
type Artifact struct {
	Parts []Part `json:"parts"`
}

// ExtractText extracts the first non-empty text from message parts.
func ExtractText(parts []Part) string {
	for _, p := range parts {
		if p.Text != "" {
			return p.Text
		}
	}
	return ""
}
