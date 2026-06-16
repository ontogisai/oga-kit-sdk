package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/ontogisai/oga-kit-sdk/auth"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// RuntimeDepsConfig configures the dependencies for DefaultRuntime.
type RuntimeDepsConfig struct {
	// GatewayURL is the Platform Access Gateway URL.
	GatewayURL string

	// EventStreamURL is the event stream connection URL.
	EventStreamURL string

	// EventStreamCreds is the path to event stream credentials.
	EventStreamCreds string

	// TokenPath is the path to the agent service token file.
	TokenPath string

	// AgentID is this agent's unique identifier.
	AgentID string

	// TenantID is the tenant this agent serves.
	TenantID string
}

// RuntimeDeps holds the connected dependencies for DefaultRuntime.
type RuntimeDeps struct {
	Gateway  *gateway.PlatformGatewayClient
	TenantID string
	AgentID  string

	tokenMgr *auth.TokenManager

	mu     sync.RWMutex
	closed bool
}

// ConnectRuntimeDeps establishes connections to platform services.
func ConnectRuntimeDeps(ctx context.Context, cfg *RuntimeDepsConfig) (*RuntimeDeps, error) {
	if cfg.GatewayURL == "" {
		return nil, fmt.Errorf("GatewayURL is required")
	}
	if cfg.TenantID == "" {
		return nil, fmt.Errorf("TenantID is required")
	}

	gw := gateway.NewPlatformGatewayClient(cfg.GatewayURL, cfg.TokenPath, cfg.TenantID)

	deps := &RuntimeDeps{
		Gateway:  gw,
		TenantID: cfg.TenantID,
		AgentID:  cfg.AgentID,
	}

	// Start sliding token renewal so the agent keeps a valid service token for
	// its whole lifetime (the initial token is short-lived; the Sidecar Manager
	// only mints the first one). The TokenManager refreshes at 50% TTL against
	// the gateway's /auth/token/refresh endpoint and rewrites the token file;
	// the gateway client reads the live token via the provider hook.
	if cfg.TokenPath != "" {
		tm, err := auth.NewTokenManager(ctx, &auth.TokenManagerConfig{
			TokenPath:  cfg.TokenPath,
			RefreshURL: strings.TrimRight(cfg.GatewayURL, "/") + "/auth/token/refresh",
		})
		if err != nil {
			// Non-fatal: fall back to the static file token. The agent still
			// works until the token expires; we log so the gap is visible.
			slog.Warn("token manager init failed; running without token rotation",
				"error", err, "token_path", cfg.TokenPath)
		} else {
			gw.SetTokenProvider(tm.Token)
			deps.tokenMgr = tm
		}
	}

	return deps, nil
}

// Close releases all resources held by the runtime dependencies.
func (d *RuntimeDeps) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	if d.tokenMgr != nil {
		d.tokenMgr.Stop()
	}
}

// StreamHandlerFunc is the pluggable streaming handler signature. The kit's
// cmd/{agent}/main.go wires a streampipeline-backed handler via
// WithStreamHandler so DefaultRuntime.HandleStream can delegate to the
// shared orchestrator without creating an import cycle from agent →
// streampipeline (the streampipeline package imports back to agent for
// shared event types).
type StreamHandlerFunc func(ctx context.Context, rt *DefaultRuntime, msg *A2AMessage, stream StreamWriter) error

// DefaultRuntime is the reference implementation of AgentRuntime.
// It provides a fully functional A2A agent with LLM reasoning, MCP tool
// calling, event stream subscription, health probes, and graceful shutdown.
type DefaultRuntime struct {
	profile *DomainAgentProfile
	deps    *RuntimeDeps
	card    *AgentCard
	planner PlannerConfig

	// streamHandler is the OGA-303 hook that delegates HandleStream to the
	// streampipeline orchestrator. Kit sidecars wire it in main.go via
	// WithStreamHandler. When nil, HandleStream uses a degraded sync
	// fallback (reason() + 3 events) so unit tests don't require the full
	// pipeline.
	streamHandler StreamHandlerFunc

	// messageHandler is the OGA-317 hook that overrides synchronous
	// HandleMessage. Kit sidecars wire the proactive-capable handler in
	// main.go via WithMessageHandler (the default proactive handler lives in
	// the streampipeline package to avoid an agent→streampipeline import
	// cycle). When nil, HandleMessage runs the built-in intent dispatch:
	// proactive_event → degraded ack (no proposal); otherwise → reactive.
	messageHandler MessageHandlerFunc

	mu    sync.RWMutex
	ready bool
}

// RuntimeOption customizes DefaultRuntime construction.
type RuntimeOption func(*DefaultRuntime)

// WithPlannerConfig overrides the default planner config.
func WithPlannerConfig(cfg PlannerConfig) RuntimeOption {
	return func(rt *DefaultRuntime) { rt.planner = cfg }
}

// WithStreamHandler injects the OGA-303 streampipeline-backed streaming
// handler. Required for production kit sidecars; without it, HandleStream
// falls back to a degraded 3-event path.
func WithStreamHandler(handler StreamHandlerFunc) RuntimeOption {
	return func(rt *DefaultRuntime) { rt.streamHandler = handler }
}

// MessageHandlerFunc is the pluggable synchronous A2A handler signature. Kits
// override via WithMessageHandler when they need custom proactive or reactive
// handling. When unset, DefaultRuntime.HandleMessage runs the built-in intent
// dispatch.
type MessageHandlerFunc func(ctx context.Context, rt *DefaultRuntime, msg *A2AMessage) (*A2AResponse, error)

// WithMessageHandler overrides the default sync message handler. Production kit
// sidecars wire the streampipeline-backed proactive handler here (the default
// proactive handler lives in streampipeline to avoid an import cycle).
func WithMessageHandler(h MessageHandlerFunc) RuntimeOption {
	return func(rt *DefaultRuntime) { rt.messageHandler = h }
}

// NewDefaultRuntime creates a new DefaultRuntime with the given profile and deps.
// Use WithStreamHandler in opts to wire the streampipeline-backed streaming
// handler (required for production OGA-303 behavior).
func NewDefaultRuntime(profile *DomainAgentProfile, deps *RuntimeDeps, opts ...RuntimeOption) *DefaultRuntime {
	return NewDefaultRuntimeWithPlanner(profile, deps, DefaultPlannerConfig(), opts...)
}

// NewDefaultRuntimeWithPlanner creates a runtime with a custom planner config.
func NewDefaultRuntimeWithPlanner(profile *DomainAgentProfile, deps *RuntimeDeps, planner PlannerConfig, opts ...RuntimeOption) *DefaultRuntime {
	skills := make([]Skill, 0, len(profile.Skills))
	for _, s := range profile.Skills {
		skills = append(skills, Skill(s))
	}

	card := &AgentCard{
		Name:        profile.Name,
		Description: profile.Description,
		URL:         fmt.Sprintf("http://localhost:%s", profile.Port),
		Version:     profile.Version,
		SupportedInterfaces: []SupportedInterface{
			{
				URL:             fmt.Sprintf("http://localhost:%s", profile.Port),
				ProtocolBinding: "JSONRPC",
				ProtocolVersion: "1.0",
			},
		},
		Capabilities:       map[string]any{},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Skills:             skills,
		Provider: &Provider{
			Organization: "ONTOGIS AI",
		},
	}

	rt := &DefaultRuntime{
		profile: profile,
		deps:    deps,
		card:    card,
		planner: planner,
		ready:   true,
	}

	for _, opt := range opts {
		opt(rt)
	}

	return rt
}

// Profile exposes the agent profile so injected stream handlers (set via
// WithStreamHandler) can read configuration like ProactiveReasoning,
// Capabilities, etc.
func (rt *DefaultRuntime) Profile() *DomainAgentProfile { return rt.profile }

// Deps exposes the runtime deps so injected stream handlers can access the
// gateway client + other shared services.
func (rt *DefaultRuntime) Deps() *RuntimeDeps { return rt.deps }

// PlannerConfig returns the active planner config so handlers can construct
// LLMToolPlanner instances with consistent settings.
func (rt *DefaultRuntime) PlannerConfig() PlannerConfig { return rt.planner }

// ServeAgentCard returns the agent's A2A card.
func (rt *DefaultRuntime) ServeAgentCard() *AgentCard {
	return rt.card
}

// HandleMessage processes a synchronous A2A message/send request.
func (rt *DefaultRuntime) HandleMessage(ctx context.Context, msg *A2AMessage) (*A2AResponse, error) {
	if msg.Params == nil || msg.Params.Message == nil {
		return nil, fmt.Errorf("message params required")
	}

	// Kit override takes precedence (e.g., the streampipeline-backed proactive
	// handler wired via WithMessageHandler).
	if rt.messageHandler != nil {
		return rt.messageHandler(ctx, rt, msg)
	}

	// Built-in intent dispatch.
	if readIntent(msg.Params.Message.Metadata) == IntentProactiveEvent {
		return rt.handleProactiveFallback(ctx, msg)
	}
	return rt.HandleReactive(ctx, msg)
}

// HandleReactive is the default synchronous path: LLM reasoning over the user's
// message text via the Platform Gateway. Exported so the streampipeline
// proactive handler (wired via WithMessageHandler) can delegate non-proactive
// messages back to the reactive path without duplicating it.
func (rt *DefaultRuntime) HandleReactive(ctx context.Context, msg *A2AMessage) (*A2AResponse, error) {
	userText := ExtractText(msg.Params.Message.Parts)
	if userText == "" {
		return nil, fmt.Errorf("message contains no text content")
	}

	slog.Info("handling message",
		"agent_id", rt.profile.AgentID,
		"tenant_id", rt.deps.TenantID,
		"text_length", len(userText),
	)

	resp, err := rt.reason(ctx, userText)
	if err != nil {
		return nil, fmt.Errorf("reasoning: %w", err)
	}

	return &A2AResponse{
		Message: &Message{
			Role:  "agent",
			Parts: []Part{{Text: resp}},
		},
	}, nil
}

// handleProactiveFallback is the degraded proactive path used when no
// proactive-capable handler is wired via WithMessageHandler. The full default
// proactive handler (parse → candidate actions → discriminated decision schema
// → RunSync[ActionDecision] → SubmitAction) lives in the streampipeline package
// because it needs the pipeline (an agent→streampipeline import would cycle).
// Production kit sidecars wire it in main.go; without it, the runtime
// acknowledges the event without submitting a proposal rather than erroring.
func (rt *DefaultRuntime) handleProactiveFallback(ctx context.Context, msg *A2AMessage) (*A2AResponse, error) {
	event, err := ParseProactiveEvent(msg)
	if err != nil {
		return nil, fmt.Errorf("parse proactive event: %w", err)
	}
	slog.WarnContext(ctx, "proactive event received but no proactive handler wired; acknowledging without proposal",
		"agent_id", rt.profile.AgentID,
		"event_type", event.EventType,
		"entity_id", event.EntityID,
	)
	return AckNoProposal(event), nil
}

// HandleStream processes a streaming A2A message/stream request.
//
// OGA-303: this method delegates to the shared streampipeline orchestrator,
// which emits the canonical event sequence (reasoning → plan → per-step
// tool_call/tool_result/citation → token-streamed artifact → consolidated
// citation → status). This is the agent's REACTIVE surface (interactive chat
// + the [Investigate] follow-up), so it always uses LLMToolPlanner — identical
// to the platform Knowledge Agent. The proactive grounding strategy is NOT
// used here; it is consumed only by the proactive message handler
// (NewProactiveMessageHandler → runProactiveReasoning). See OGA-348.
//
// This is implemented inline rather than as a direct streampipeline import
// because that would create an import cycle (the agent package's streampipeline
// subpackage already imports back to agent for shared types). Production
// wiring uses cmd/agent-runtime/main.go to construct DefaultRuntime which
// then routes here.
func (rt *DefaultRuntime) HandleStream(ctx context.Context, msg *A2AMessage, stream StreamWriter) error {
	if msg.Params == nil || msg.Params.Message == nil {
		return fmt.Errorf("message params required")
	}

	userText := ExtractText(msg.Params.Message.Parts)
	if userText == "" {
		return fmt.Errorf("message contains no text content")
	}

	// rt.streamHandler is wired by cmd/agent-runtime/main.go (or kit
	// sidecar main.go) to a streampipeline-backed handler. When unset
	// (e.g., minimal test fixtures), fall back to a degraded sync path
	// that emits one artifact event with the full LLM answer.
	if rt.streamHandler != nil {
		return rt.streamHandler(ctx, rt, msg, stream)
	}

	return rt.handleStreamFallback(ctx, userText, stream)
}

// handleStreamFallback is the degraded sync path used when no streamHandler
// is wired. Mirrors the pre-OGA-303 3-event behavior so unit tests of the
// runtime that don't construct a full streampipeline still work.
func (rt *DefaultRuntime) handleStreamFallback(ctx context.Context, userText string, stream StreamWriter) error {
	// Working status
	if err := stream.WriteEvent(ctx, &StreamEvent{
		Type:    EventTypeStatus,
		Payload: &StatusPayload{State: TaskStateWorking},
	}); err != nil {
		return err
	}

	resp, err := rt.reason(ctx, userText)
	if err != nil {
		_ = stream.WriteEvent(ctx, &StreamEvent{
			Type: EventTypeStatus,
			Payload: &StatusPayload{
				State: TaskStateFailed,
				Error: &StatusError{Code: -32000, Message: err.Error()},
			},
		})
		return err
	}

	if err := stream.WriteEvent(ctx, &StreamEvent{
		Type:    EventTypeArtifact,
		Payload: &ArtifactPayload{Parts: []ArtifactPart{{Text: resp}}},
	}); err != nil {
		return err
	}

	if err := stream.WriteEvent(ctx, &StreamEvent{
		Type:    EventTypeStatus,
		Payload: &StatusPayload{State: TaskStateCompleted},
	}); err != nil {
		return err
	}

	return stream.Close()
}

// Healthz returns nil if the agent is alive.
func (rt *DefaultRuntime) Healthz(_ context.Context) error {
	return nil
}

// Readyz returns nil if the agent is ready to serve traffic.
func (rt *DefaultRuntime) Readyz(_ context.Context) error {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if !rt.ready {
		return fmt.Errorf("agent not ready")
	}
	return nil
}

// reason performs LLM reasoning via the Platform Gateway, with MCP tool
// calling when the profile declares tools the LLM can use.
func (rt *DefaultRuntime) reason(ctx context.Context, userText string) (string, error) {
	answer, results, err := PlanAndExecute(ctx, rt.deps.Gateway, rt.profile, userText, rt.planner)
	if err != nil {
		return "", err
	}
	if len(results) > 0 {
		successful := 0
		for _, r := range results {
			if r.Success {
				successful++
			}
		}
		slog.Info("agent: plan executed",
			"agent_id", rt.profile.AgentID,
			"tenant_id", rt.deps.TenantID,
			"steps_total", len(results),
			"steps_succeeded", successful,
		)
	}
	return answer, nil
}
