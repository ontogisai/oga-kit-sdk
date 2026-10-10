// Package agent — typed StreamEvent envelope and payload structs (OGA-303).
//
// This file is the single source of truth for the streaming-event wire schema
// shared between the platform's Frontier (HTTPStreamingInvoker), the Knowledge
// Agent (in-process via streampipeline), and every kit-supplied domain agent
// sidecar (HTTP/SSE via streampipeline).
//
// Wire format: SSE event with `event: {Type}\ndata: {JSON-marshalled StreamEvent}\n\n`.
// The platform's HTTPStreamingInvoker and the kit-side httpStreamWriter (in serve.go)
// agree on this format. Field names + JSON tags match the platform's pre-OGA-303
// internal/agent/stream_types.go byte-for-byte so the migration is purely a relocation.
package agent

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// EventType is the discriminator for streaming event payloads.
type EventType string

const (
	// EventTypeStatus signals task lifecycle transitions (working, completed, failed, canceled).
	EventTypeStatus EventType = "task/status"

	// EventTypeReasoning carries LLM-generated narrative (token-batched, incremental).
	EventTypeReasoning EventType = "task/reasoning"

	// EventTypePlan describes ordered tool/agent steps with dependencies.
	EventTypePlan EventType = "task/plan"

	// EventTypeDecision records routing, fallback, or retry decisions (Frontier only).
	EventTypeDecision EventType = "task/decision"

	// EventTypeAgentCall signals the start of a sub-agent invocation (Frontier only).
	EventTypeAgentCall EventType = "task/agent_call"

	// EventTypeAgentResult signals the completion of a sub-agent invocation (Frontier only).
	EventTypeAgentResult EventType = "task/agent_result"

	// EventTypeToolCall signals the start of an MCP tool invocation.
	EventTypeToolCall EventType = "task/tool_call"

	// EventTypeToolResult signals the completion of an MCP tool invocation.
	EventTypeToolResult EventType = "task/tool_result"

	// EventTypeCitation lists data sources accessed during processing.
	EventTypeCitation EventType = "task/citation"

	// EventTypeArtifact carries the final response content (token-streamed).
	EventTypeArtifact EventType = "task/artifact"

	// EventTypeUsage carries LLM token usage for one invocation in the ReAct
	// loop (a per-turn decision call or the terminal assembly call) or the
	// per-request aggregate (OGA-420). Distinct event so usage never bloats the
	// content payloads and consumers can meter cost without parsing prose.
	EventTypeUsage EventType = "task/usage"
)

// Task lifecycle states emitted in StatusPayload.State.
const (
	TaskStateWorking   = "working"
	TaskStateCompleted = "completed"
	TaskStateFailed    = "failed"
	TaskStateCanceled  = "canceled"
	// TaskStateInputRequired is the canonical A2A state for "the agent paused and
	// needs the user to answer before it can continue" (OGA-446). Reactive-only:
	// the proactive path never emits it (it has no user to ask). The turn carries
	// a ClarificationPayload on the StatusPayload and executes no mutating tool.
	TaskStateInputRequired = "input-required"
)

// Clarification kinds carried on ClarificationPayload.Kind.
const (
	ClarifyKindDisambiguation = "disambiguation" // multiple candidate targets matched
	ClarifyKindMissingField   = "missing_field"  // a required argument is unknown
	ClarifyKindConfirmation   = "confirmation"   // confirm a mutating action before writing
)

// ClarifyOption is one discrete choice offered to the user in a clarification
// turn. Richer channels render these as quick-reply buttons; basic channels fall
// back to the question text.
type ClarifyOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ClarificationPayload is both the terminal task/status{input-required} payload
// and the wire form of the pending_action_context continuation token (OGA-446).
// The agent emits it to ask the user a question (disambiguation, a missing
// required field, or a pre-write confirmation); the platform persists it and
// re-injects it on the follow-up turn so the same agent resumes deterministically.
type ClarificationPayload struct {
	Question         string          `json:"question"`
	Kind             string          `json:"kind,omitempty"`
	MissingFields    []string        `json:"missing_fields,omitempty"`
	Options          []ClarifyOption `json:"options,omitempty"`
	PendingTool      string          `json:"pending_tool,omitempty"`
	PartialArguments map[string]any  `json:"partial_arguments,omitempty"`
}

// StreamEvent is the universal envelope for all SSE events emitted during
// agent streaming. Every event carries hierarchical span information enabling
// clients to reconstruct the full reasoning tree.
type StreamEvent struct {
	// TaskID is the unique identifier for the streaming task (UUID v4).
	TaskID string `json:"task_id"`

	// Sequence is a monotonically increasing integer starting at 1.
	Sequence int `json:"sequence"`

	// Timestamp is the UTC time when the event was generated.
	Timestamp time.Time `json:"timestamp"`

	// SpanID identifies this unit of work within the task tree.
	SpanID string `json:"span_id"`

	// ParentSpanID links to the parent span. Empty string for root spans.
	ParentSpanID string `json:"parent_span_id,omitempty"`

	// Depth is the 0-based nesting level (root=0, sub-agent=1, tool=2).
	Depth int `json:"depth"`

	// Actor identifies who generated this event.
	Actor EventActor `json:"actor"`

	// Type is the event type discriminator.
	Type EventType `json:"type"`

	// Payload is the type-specific event data.
	Payload any `json:"payload"`

	// TraceID is the OpenTelemetry trace ID, included on root span's first event.
	TraceID string `json:"trace_id,omitempty"`
}

// EventActor identifies the source of a streaming event.
type EventActor struct {
	// Type categorizes the actor: "frontier", "sub_agent", "domain_agent", "tool".
	Type string `json:"type"`

	// ID is the stable identifier: "frontier-agent", "knowledge-agent", "mcp:kg_search".
	ID string `json:"id"`

	// DisplayName is the human-readable name for UI rendering.
	DisplayName string `json:"display_name"`
}

// --- Event Payloads ---

// StatusPayload carries task lifecycle state transitions.
type StatusPayload struct {
	// State is one of: working, completed, failed, canceled, input-required.
	State string `json:"state"`

	// Error is present only when State is "failed".
	Error *StatusError `json:"error,omitempty"`

	// Clarification is present only when State is "input-required" (OGA-446): the
	// agent paused to ask the user a question. It is the pending_action_context
	// the platform persists and re-injects on the resume turn.
	Clarification *ClarificationPayload `json:"clarification,omitempty"`
}

// StatusError describes a failure within a streaming task.
type StatusError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ReasoningPayload carries LLM-generated narrative text.
type ReasoningPayload struct {
	// Text is the reasoning content (may be a partial chunk).
	Text string `json:"text"`

	// Append indicates whether this is an incremental chunk (true) or complete (false).
	Append bool `json:"append"`
}

// PlanPayload describes ordered execution steps.
type PlanPayload struct {
	Steps []PlanStep `json:"steps"`
}

// PlanStep is a single step in an execution plan.
type PlanStep struct {
	// Index is the 0-based step position.
	Index int `json:"index"`

	// Description is a human-readable explanation of the step.
	Description string `json:"description"`

	// Tool is the MCP tool name (empty if the step is an agent call).
	Tool string `json:"tool,omitempty"`

	// DependsOn is the index of the step this depends on (-1 for no dependency).
	DependsOn int `json:"depends_on"`

	// Skipped indicates the step was skipped (e.g., conditional false).
	Skipped bool `json:"skipped,omitempty"`

	// SkipReason explains why a skipped step was skipped.
	SkipReason string `json:"skip_reason,omitempty"`
}

// DecisionPayload records a routing, fallback, or retry decision.
type DecisionPayload struct {
	// DecisionType is one of: routing, fallback, retry.
	DecisionType string `json:"decision_type"`

	// Chosen is the selected option.
	Chosen string `json:"chosen"`

	// Alternatives lists other options that were considered.
	Alternatives []string `json:"alternatives"`

	// Rationale explains why this choice was made.
	Rationale string `json:"rationale"`
}

// AgentCallPayload signals the start of a sub-agent invocation.
type AgentCallPayload struct {
	TargetAgent       string `json:"target_agent"`
	AgentType         string `json:"agent_type"`
	PriorityLevel     int    `json:"priority_level"`
	MatchedCapability string `json:"matched_capability"`
	Task              string `json:"task"`
	SupportsStreaming bool   `json:"supports_streaming"`
}

// AgentResultPayload signals the completion of a sub-agent invocation.
type AgentResultPayload struct {
	AgentID   string `json:"agent_id"`
	Success   bool   `json:"success"`
	Summary   string `json:"summary"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// ToolCallPayload signals the start of an MCP tool invocation.
type ToolCallPayload struct {
	// ToolName is the MCP tool being invoked.
	ToolName string `json:"tool_name"`

	// Arguments are the tool input parameters.
	Arguments map[string]any `json:"arguments"`

	// StepIndex links this call to the corresponding plan step.
	StepIndex int `json:"step_index"`

	// Skipped is true when the step was skipped before execution.
	Skipped bool `json:"skipped,omitempty"`

	// SkipReason explains why a skipped step was skipped.
	SkipReason string `json:"skip_reason,omitempty"`
}

// ToolResultPayload signals the completion of an MCP tool invocation.
type ToolResultPayload struct {
	StepIndex       int    `json:"step_index"`
	ToolName        string `json:"tool_name"`
	Success         bool   `json:"success"`
	Summary         string `json:"summary"`
	ResultPreview   string `json:"result_preview"`
	ResultSizeBytes int    `json:"result_size_bytes"`
	Truncated       bool   `json:"truncated"`
	LatencyMs       int64  `json:"latency_ms"`
	ErrorCode       string `json:"error_code,omitempty"`
}

// CitationPayload lists data sources accessed during processing.
type CitationPayload struct {
	Sources []CitationSource `json:"sources"`
}

// CitationSource identifies a single data source used in the response.
type CitationSource struct {
	// Type categorizes the source: entity, ontology_version, time_range, h3_cells, document.
	Type string `json:"type"`

	// ID is the stable identifier for the source.
	ID string `json:"id"`

	// Label is a human-readable name for the source.
	Label string `json:"label"`

	// ValidFrom is the bi-temporal valid_from (ISO 8601, optional).
	ValidFrom string `json:"valid_from,omitempty"`

	// ValidTo is the bi-temporal valid_to (ISO 8601, optional).
	ValidTo string `json:"valid_to,omitempty"`

	// H3Cells lists the H3 cell IDs accessed (for spatial sources).
	H3Cells []string `json:"h3_cells,omitempty"`

	// Resolution is the H3 resolution level (for spatial sources).
	Resolution int `json:"resolution,omitempty"`
}

// ArtifactPayload carries the final response content.
type ArtifactPayload struct {
	// Parts contains the content parts (text chunks).
	Parts []ArtifactPart `json:"parts"`

	// Append indicates whether this extends a previous artifact (true) or replaces (false).
	Append bool `json:"append"`

	// Metadata carries additional context about the artifact.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ArtifactPart is a single content part within an artifact.
type ArtifactPart struct {
	// Text is the content text.
	Text string `json:"text,omitempty"`
}

// TokenUsage reports LLM token consumption for a single invocation or an
// aggregate (OGA-420). Mirrors the gateway Usage shape but lives in the agent
// package so the wire schema has one home alongside the StreamEvent envelope.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add returns the element-wise sum of two usages (for the per-request aggregate).
func (u TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.PromptTokens + o.PromptTokens,
		CompletionTokens: u.CompletionTokens + o.CompletionTokens,
		TotalTokens:      u.TotalTokens + o.TotalTokens,
	}
}

// Usage event roles.
const (
	// UsageRoleDecision is one per-turn ReAct decision call.
	UsageRoleDecision = "decision"
	// UsageRoleAssembly is the terminal assembly call.
	UsageRoleAssembly = "assembly"
	// UsageRoleAggregate is the per-request sum of all decision + assembly calls.
	UsageRoleAggregate = "aggregate"
)

// UsagePayload carries token usage for one LLM invocation (or the aggregate).
type UsagePayload struct {
	// Role is one of: decision, assembly, aggregate.
	Role string `json:"role"`

	// TurnIndex is the 0-based ReAct turn for a decision call; -1 for assembly
	// and aggregate.
	TurnIndex int `json:"turn_index"`

	// Model is the model that served the call, when known (empty = gateway default).
	Model string `json:"model,omitempty"`

	// Usage is the token counts. Zero values when Available is false.
	Usage TokenUsage `json:"usage"`

	// Available is false when the proxy returned no usage for this call — the
	// counts are then zero and MUST NOT be read as "0 tokens" (OGA-420: no
	// fabricated usage). True when the counts are real.
	Available bool `json:"available"`
}

// --- SpanTracker ---

// SpanTracker generates and tracks hierarchical span relationships for
// streaming events. It maintains a parent-child tree enabling clients to
// reconstruct the full reasoning hierarchy.
//
// Span hierarchy:
//   - Root (depth=0): the overall request
//   - Child (depth=1): sub-agent invocations
//   - Grandchild (depth=2): tool calls within sub-agents
type SpanTracker struct {
	rootSpanID string
	traceID    string

	mu      sync.RWMutex
	parents map[string]string
}

// NewSpanTracker creates a tracker with a new root span and the given
// OpenTelemetry trace ID for distributed trace correlation.
func NewSpanTracker(traceID string) *SpanTracker {
	rootID := uuid.New().String()
	st := &SpanTracker{
		rootSpanID: rootID,
		traceID:    traceID,
		parents:    make(map[string]string),
	}
	st.parents[rootID] = ""
	return st
}

// RootSpan returns the root span_id (depth=0, no parent).
func (st *SpanTracker) RootSpan() string { return st.rootSpanID }

// TraceID returns the OpenTelemetry trace ID associated with this tracker.
func (st *SpanTracker) TraceID() string { return st.traceID }

// ChildSpan creates a new span_id with the given parent and records the
// relationship. Thread-safe.
func (st *SpanTracker) ChildSpan(parentSpanID string) string {
	childID := uuid.New().String()
	st.mu.Lock()
	st.parents[childID] = parentSpanID
	st.mu.Unlock()
	return childID
}

// Depth computes the depth of a span by walking the parent chain.
func (st *SpanTracker) Depth(spanID string) int {
	st.mu.RLock()
	defer st.mu.RUnlock()

	depth := 0
	current := spanID

	for {
		parent, exists := st.parents[current]
		if !exists {
			return -1
		}
		if parent == "" {
			return depth
		}
		depth++
		current = parent

		if depth > 100 {
			return -1
		}
	}
}

// ParentOf returns the parent span ID for the given span.
func (st *SpanTracker) ParentOf(spanID string) string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.parents[spanID]
}

// IsRoot returns true if the given span is the root span.
func (st *SpanTracker) IsRoot(spanID string) bool { return spanID == st.rootSpanID }
