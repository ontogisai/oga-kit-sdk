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
// Planner selection is profile-driven:
//   - When profile.ProactiveReasoning.GroundingStrategy is non-empty,
//     uses GroundingStrategyPlanner (deterministic, no LLM planning call).
//   - Otherwise uses LLMToolPlanner (dynamic per-request planning).
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

		// Pick planner based on profile.
		var planner StreamPlanner
		if profile != nil && profile.ProactiveReasoning != nil && len(profile.ProactiveReasoning.GroundingStrategy) > 0 {
			planner = NewGroundingStrategyPlanner(profile)
		} else {
			planner = NewLLMToolPlanner(deps.Gateway, profile, rt.PlannerConfig())
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
