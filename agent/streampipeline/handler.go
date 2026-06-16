package streampipeline

import (
	"context"
	"encoding/json"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// NewDefaultStreamHandler returns a StreamHandlerFunc that drives the
// shared streampipeline orchestrator. Wire it into DefaultRuntime via
// agent.WithStreamHandler:
//
//	runtime := agent.NewDefaultRuntime(profile, deps,
//	    agent.WithStreamHandler(streampipeline.NewDefaultStreamHandler(streampipeline.DefaultConfig())),
//	)
//
// The streaming path is the agent's REACTIVE surface — interactive operator
// chat and the [Investigate] deep link (Frontier routes the follow-up here as
// a free-text A2A message with intent="investigation"). It selects the planner
// per request:
//
//   - When the inbound message carries concrete investigation entity ids
//     (InvestigationContext.trigger_entity_ids, surfaced in the message
//     metadata), it uses the deterministic InvestigationGroundingPlanner to
//     seed grounded retrieval from those entities (OGA-378).
//   - Otherwise (plain chat) it uses LLMToolPlanner, like the platform's
//     Knowledge Agent: the LLM plans MCP tools dynamically per request.
//
// The profile's proactive_reasoning.grounding_strategy is deliberately NOT
// consulted here. A grounding strategy is a deterministic plan tuned for an
// autonomous proactive event (it references event placeholders like
// {entity_id} that only exist on the proactive event). Running it on the
// reactive path would replay that rigid plan against an interactive query —
// the placeholders pass through literally and tool calls fail (OGA-348). The
// investigation planner above is the reactive analogue: it takes CONCRETE ids
// (no placeholders). The grounding strategy is consumed exclusively by the
// proactive handler (NewProactiveMessageHandler → runProactiveReasoning).
//
// All MCP tool calls and LLM completions go through deps.Gateway —
// the Platform Access Gateway — for centralised PBAC, audit, rate
// limiting, and tenant attribution.
func NewDefaultStreamHandler(cfg Config) agent.StreamHandlerFunc {
	if cfg.ToolTimeout == 0 {
		cfg = DefaultConfig()
	}
	pipeline := NewPipeline()

	return func(ctx context.Context, rt *agent.DefaultRuntime, msg *agent.A2AMessage, stream agent.StreamWriter) error {
		userText := agent.ExtractText(msg.Params.Message.Parts)

		profile := rt.Profile()
		deps := rt.Deps()

		// Select the reactive planner per request. When the investigation
		// forward carries concrete entity ids (OGA-378), ground deterministically
		// on them; otherwise plan tools dynamically via the LLM. The proactive
		// grounding strategy is never used here (see the doc comment + OGA-348).
		investigationIDs := investigationEntityIDsFromMessage(msg.Params.Message)
		var planner StreamPlanner
		if len(investigationIDs) > 0 {
			planner = NewInvestigationGroundingPlanner(investigationIDs)
		} else {
			planner = reactiveStreamPlanner(rt)
		}

		// Determine actor identity.
		actor := agent.EventActor{
			Type:        "domain_agent",
			ID:          profile.AgentID,
			DisplayName: profile.Name,
		}

		// Determine assembly prompt from the profile's proactive system prompt.
		assemblyPrompt := ""
		if profile.ProactiveReasoning != nil {
			assemblyPrompt = profile.ProactiveReasoning.SystemPrompt
		}

		input := Input{
			Query:                  userText,
			TenantID:               deps.TenantID,
			PrincipalID:            "", // populated by gateway on outbound calls
			Actor:                  actor,
			AssemblyPrompt:         assemblyPrompt,
			ToolNames:              agent.UniqueTools(profile),
			InvestigationEntityIDs: investigationIDs,
		}

		// Bridge: streampipeline emits to a channel; we forward to the
		// StreamWriter. Channel buffered enough to absorb a typical run.
		events := make(chan *agent.StreamEvent, 32)
		runErr := make(chan error, 1)

		pipelineDeps := Deps{
			Gateway: deps.Gateway,
			Config:  cfg,
		}

		go func() {
			err := pipeline.Run(ctx, pipelineDeps, input, planner, events)
			close(events)
			runErr <- err
		}()

		for evt := range events {
			if err := stream.WriteEvent(ctx, evt); err != nil {
				// Drain remaining events to let the goroutine finish.
				for range events {
				}
				<-runErr
				return err
			}
		}

		err := <-runErr
		if closeErr := stream.Close(); err == nil {
			return closeErr
		}
		return err
	}
}

// reactiveStreamPlanner returns the StreamPlanner used by the reactive
// streaming path. It ALWAYS returns an LLMToolPlanner — the reactive surface
// (interactive chat + the [Investigate] follow-up) plans tools dynamically per
// request, exactly like the platform Knowledge Agent.
//
// It deliberately ignores profile.ProactiveReasoning.GroundingStrategy: a
// grounding strategy is a deterministic plan tuned for an autonomous proactive
// event and references event placeholders (e.g. {entity_id}) that do not exist
// on a reactive query. The grounding strategy is consumed only by the proactive
// handler (NewProactiveMessageHandler → runProactiveReasoning). See OGA-348.
func reactiveStreamPlanner(rt *agent.DefaultRuntime) StreamPlanner {
	return NewLLMToolPlanner(rt.Deps().Gateway, rt.Profile(), rt.PlannerConfig())
}

// Metadata keys carrying investigation context on an inbound A2A message
// (set by Frontier when it force-routes an [Investigate] follow-up). These
// mirror the platform's stateless-investigation contract
// (internal/agent/investigation_stateless.go) by value.
const (
	metadataKeyInvestigationContext = "investigation_context"
	metadataKeyTriggerEntityIDs     = "trigger_entity_ids"
)

// investigationEntityIDsFromMessage extracts the concrete investigation seed
// entity ids from an inbound A2A message's metadata (OGA-378). Frontier, when
// it force-routes an [Investigate] follow-up to the proposing agent, carries
// the proposal's InvestigationContext in message metadata. Two shapes are
// accepted (in priority order):
//
//  1. metadata["investigation_context"] — a JSON string of the
//     InvestigationContextPayload; we read its trigger_entity_ids.
//  2. metadata["trigger_entity_ids"]    — a direct array of ids (or a single
//     string), for callers that forward only the ids.
//
// Returns nil when neither is present or no ids are found — the caller then
// falls back to the LLM planner (plain chat).
func investigationEntityIDsFromMessage(m *agent.Message) []string {
	if m == nil || m.Metadata == nil {
		return nil
	}

	// Shape 1: the serialized investigation context.
	if raw, ok := m.Metadata[metadataKeyInvestigationContext]; ok {
		if s, isStr := raw.(string); isStr && s != "" {
			var ic gateway.InvestigationContextPayload
			if err := json.Unmarshal([]byte(s), &ic); err == nil && len(ic.TriggerEntityIDs) > 0 {
				return ic.TriggerEntityIDs
			}
		}
	}

	// Shape 2: a direct ids field.
	if raw, ok := m.Metadata[metadataKeyTriggerEntityIDs]; ok {
		return coerceStringSlice(raw)
	}
	return nil
}

// coerceStringSlice converts a metadata value that may be []any, []string, or
// a single string into a []string (dropping non-string / empty elements).
func coerceStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
