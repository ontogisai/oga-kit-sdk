package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

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

	return &RuntimeDeps{
		Gateway:  gw,
		TenantID: cfg.TenantID,
		AgentID:  cfg.AgentID,
	}, nil
}

// Close releases all resources held by the runtime dependencies.
func (d *RuntimeDeps) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}

// DefaultRuntime is the reference implementation of AgentRuntime.
// It provides a fully functional A2A agent with LLM reasoning, MCP tool
// calling, event stream subscription, health probes, and graceful shutdown.
type DefaultRuntime struct {
	profile *DomainAgentProfile
	deps    *RuntimeDeps
	card    *AgentCard
	planner PlannerConfig

	mu    sync.RWMutex
	ready bool
}

// NewDefaultRuntime creates a new DefaultRuntime with the given profile and deps.
func NewDefaultRuntime(profile *DomainAgentProfile, deps *RuntimeDeps) *DefaultRuntime {
	return NewDefaultRuntimeWithPlanner(profile, deps, DefaultPlannerConfig())
}

// NewDefaultRuntimeWithPlanner creates a runtime with a custom planner config.
func NewDefaultRuntimeWithPlanner(profile *DomainAgentProfile, deps *RuntimeDeps, planner PlannerConfig) *DefaultRuntime {
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

	return rt
}

// ServeAgentCard returns the agent's A2A card.
func (rt *DefaultRuntime) ServeAgentCard() *AgentCard {
	return rt.card
}

// HandleMessage processes a synchronous A2A message/send request.
func (rt *DefaultRuntime) HandleMessage(ctx context.Context, msg *A2AMessage) (*A2AResponse, error) {
	if msg.Params == nil || msg.Params.Message == nil {
		return nil, fmt.Errorf("message params required")
	}

	userText := ExtractText(msg.Params.Message.Parts)
	if userText == "" {
		return nil, fmt.Errorf("message contains no text content")
	}

	slog.Info("handling message",
		"agent_id", rt.profile.AgentID,
		"tenant_id", rt.deps.TenantID,
		"text_length", len(userText),
	)

	// Use LLM via Platform Gateway for reasoning
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

// HandleStream processes a streaming A2A message/stream request.
func (rt *DefaultRuntime) HandleStream(ctx context.Context, msg *A2AMessage, stream StreamWriter) error {
	if msg.Params == nil || msg.Params.Message == nil {
		return fmt.Errorf("message params required")
	}

	userText := ExtractText(msg.Params.Message.Parts)
	if userText == "" {
		return fmt.Errorf("message contains no text content")
	}

	// Send status event
	statusData, _ := json.Marshal(map[string]string{"state": "running"})
	if err := stream.WriteEvent(ctx, &StreamEvent{Type: "status", Data: statusData}); err != nil {
		return err
	}

	// Reason and stream result
	resp, err := rt.reason(ctx, userText)
	if err != nil {
		errData, _ := json.Marshal(map[string]string{"error": err.Error()})
		_ = stream.WriteEvent(ctx, &StreamEvent{Type: "error", Data: errData})
		return err
	}

	msgData, _ := json.Marshal(Message{
		Role:  "agent",
		Parts: []Part{{Text: resp}},
	})
	if err := stream.WriteEvent(ctx, &StreamEvent{Type: "message", Data: msgData}); err != nil {
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
