package streampipeline

import (
	"context"

	"github.com/ontogisai/oga-kit-sdk/agent"
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
// a free-text A2A message with intent="investigation"). Like the platform's
// Knowledge Agent, it ALWAYS uses LLMToolPlanner: the LLM reads the operator's
// message plus any injected investigation context and plans MCP tools
// dynamically per request.
//
// The profile's proactive_reasoning.grounding_strategy is deliberately NOT
// consulted here. A grounding strategy is a deterministic plan tuned for an
// autonomous proactive event (it references event placeholders like
// {entity_id} that only exist on the proactive event). Running it on the
// reactive path would replay that rigid plan against an interactive query —
// the placeholders pass through literally and tool calls fail (OGA-348). The
// grounding strategy is consumed exclusively by the proactive handler
// (NewProactiveMessageHandler → runProactiveReasoning), which constructs its
// own GroundingStrategyPlanner.
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

		// Reactive streaming always uses LLM-driven tool planning — identical
		// to the Knowledge Agent. The grounding strategy is proactive-only
		// (see the doc comment above and runProactiveReasoning in proactive.go).
		planner := reactiveStreamPlanner(rt)

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
			Query:          userText,
			TenantID:       deps.TenantID,
			PrincipalID:    "", // populated by gateway on outbound calls
			Actor:          actor,
			AssemblyPrompt: assemblyPrompt,
			ToolNames:      agent.UniqueTools(profile),
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
