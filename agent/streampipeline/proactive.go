package streampipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// NewProactiveMessageHandler returns the SDK's default proactive message
// handler, wired into a DefaultRuntime via agent.WithMessageHandler in a kit's
// main.go. It lives here (not on DefaultRuntime) because it needs the pipeline,
// and an agent→streampipeline import would cycle — the same reason
// NewDefaultStreamHandler lives here.
//
// Flow for a proactive_event message:
//  1. parse the event,
//  2. gather the candidate action catalog,
//  3. compose a discriminated JSON-Schema decision (oneOf over candidates +
//     no_action),
//  4. run the grounding strategy and let the reasoning LLM choose one action
//     (or decline) via RunSync[agent.ActionDecision],
//  5. pack the chosen action's governance fields + the profile routing into a
//     SubmitActionInput and submit it.
//
// Non-proactive messages delegate to rt.HandleReactive so wiring this handler
// does not disable the reactive path.
func NewProactiveMessageHandler() agent.MessageHandlerFunc {
	return func(ctx context.Context, rt *agent.DefaultRuntime, msg *agent.A2AMessage) (*agent.A2AResponse, error) {
		if msg.Params == nil || msg.Params.Message == nil {
			return nil, fmt.Errorf("message params required")
		}
		if intent, _ := msg.Params.Message.Metadata["intent"].(string); intent != agent.IntentProactiveEvent {
			return rt.HandleReactive(ctx, msg)
		}
		return handleProactive(ctx, rt, msg)
	}
}

func handleProactive(ctx context.Context, rt *agent.DefaultRuntime, msg *agent.A2AMessage) (*agent.A2AResponse, error) {
	event, err := agent.ParseProactiveEvent(msg)
	if err != nil {
		return nil, fmt.Errorf("parse proactive event: %w", err)
	}
	profile := rt.Profile()
	candidates := profile.CandidateActions(event)
	if len(candidates) == 0 {
		slog.InfoContext(ctx, "no candidate actions for proactive event; no proposal",
			"agent_id", profile.AgentID, "event_type", event.EventType)
		return agent.AckNoProposal(event), nil
	}

	// Fast-ack + async. The Event Router invokes proactive events over a
	// synchronous A2A message/send bounded by a SHORT client timeout
	// (configs/event-router.yaml a2a_timeout, default 5s — "domain agents
	// should acknowledge quickly and process async"). Grounding + LLM
	// reasoning + SubmitAction routinely exceed that window; running them on
	// the inbound request context means the router's timeout cancels the work
	// before any proposal is submitted (OGA-343). So we acknowledge receipt
	// immediately and run the reasoning on a DETACHED context whose lifetime
	// is the agent's own reasoning budget, not the router's delivery-ack
	// window. context.WithoutCancel preserves request-scoped values (tenant,
	// locale) while dropping the router's cancellation.
	bgctx := context.WithoutCancel(ctx)
	go func() {
		rctx, cancel := context.WithTimeout(bgctx, proactiveBudget(profile))
		defer cancel()
		if rerr := runProactiveReasoning(rctx, rt, event, candidates); rerr != nil {
			slog.ErrorContext(rctx, "proactive reasoning failed",
				"agent_id", profile.AgentID,
				"event_type", event.EventType,
				"entity_id", event.EntityID,
				"error", rerr)
		}
	}()

	slog.InfoContext(ctx, "proactive event accepted; reasoning asynchronously",
		"agent_id", profile.AgentID, "event_type", event.EventType, "entity_id", event.EntityID)
	return agent.AckAccepted(event), nil
}

// runProactiveReasoning runs the synchronous grounding strategy + LLM action
// decision and submits the chosen action. It is invoked on a detached context
// from handleProactive's background goroutine, so it owns all logging of its
// outcome — no caller observes its return value.
func runProactiveReasoning(ctx context.Context, rt *agent.DefaultRuntime, event *agent.ProactiveEvent, candidates []agent.ActionDef) error {
	profile := rt.Profile()
	schema, err := buildActionDecisionSchema(candidates)
	if err != nil {
		return fmt.Errorf("build action decision schema: %w", err)
	}

	deps := Deps{Gateway: rt.Deps().Gateway, Logger: slog.Default(), Config: DefaultConfig()}
	persona := ""
	var grounding []agent.GroundingStep
	if profile.ProactiveReasoning != nil {
		persona = profile.ProactiveReasoning.SystemPrompt
		grounding = profile.ProactiveReasoning.GroundingStrategy
	}
	input := Input{
		Query:          proactiveQuery(event),
		TenantID:       event.TenantID,
		Actor:          agent.EventActor{Type: "domain_agent", ID: profile.AgentID, DisplayName: profile.Name},
		AssemblyPrompt: proactiveAssemblyPrompt(profile, candidates),
		// Persona + palette for the planner. The proactive palette is the
		// profile tool union (kg_* + Tier-3) ONLY — it never contains an
		// agent-delegation capability (OGA-419 Property 5). The grounding
		// strategy is passed as ADVISORY hints (same struct as the profile),
		// and the resolved event facts seed the planner so it derives concrete
		// tool arguments without {placeholder} substitution.
		Persona:           PlannerPersona{SystemPrompt: persona, Tools: agent.UniqueTools(profile)},
		GroundingStrategy: grounding,
		SeedFacts:         proactiveSeedFacts(event),
	}
	planner := NewLLMToolPlanner(deps.Gateway, agent.DefaultPlannerConfig())

	decision, _, usage, usageAvail, err := RunSyncWithUsage[agent.ActionDecision](ctx, NewPipeline(), deps, input, planner, schema)
	// Log the proactive reasoning's token cost regardless of outcome (OGA-420
	// Gap 2/4): the proactive stream→collect path has no UI consumer, so this
	// always-on aggregate line is the only place its cost is observable. Emitted
	// even on error (tokens were still spent) and labelled when the proxy
	// reported no usage. The per-turn breakdown is available under
	// OGA_PROACTIVE_REACT_LOG.
	if usageAvail {
		slog.InfoContext(ctx, "proactive reasoning token usage",
			"agent_id", profile.AgentID,
			"tenant_id", event.TenantID,
			"event_type", event.EventType,
			"entity_id", event.EntityID,
			"tokens_in", usage.PromptTokens,
			"tokens_out", usage.CompletionTokens,
			"tokens_total", usage.TotalTokens,
		)
	}
	if err != nil {
		return fmt.Errorf("proactive reasoning: %w", err)
	}

	if decision.ActionType == "" || decision.ActionType == agent.ActionNoOp {
		slog.InfoContext(ctx, "agent reasoned no action warranted",
			"agent_id", profile.AgentID, "event_type", event.EventType, "rationale", decision.Reasoning)
		return nil
	}

	action, ok := profile.Action(decision.ActionType)
	if !ok {
		return fmt.Errorf("%w: LLM chose unknown action %q", agent.ErrActionDecision, decision.ActionType)
	}

	in := buildSubmitActionInput(profile, action, event, &decision)
	submission, err := rt.Deps().Gateway.SubmitAction(ctx, in)
	if err != nil {
		return fmt.Errorf("submit action: %w", err)
	}
	slog.InfoContext(ctx, "proactive action proposal submitted",
		"agent_id", profile.AgentID,
		"action", decision.ActionType,
		"workflow_id", submission.WorkflowID,
		"event_type", event.EventType,
		"entity_id", event.EntityID)
	return nil
}

// proactiveBudget derives the detached reasoning timeout from the profile's
// proactive_reasoning timeouts (context gather + reasoning) plus headroom for
// SubmitAction. Falls back to a generous default when the profile leaves them
// unset.
func proactiveBudget(p *agent.DomainAgentProfile) time.Duration {
	const fallback = 120 * time.Second
	if p == nil || p.ProactiveReasoning == nil {
		return fallback
	}
	total := durationOrZero(p.ProactiveReasoning.ContextGatherTimeout) +
		durationOrZero(p.ProactiveReasoning.ReasoningTimeout)
	if total <= 0 {
		return fallback
	}
	return total + 30*time.Second // headroom for SubmitAction
}

// buildActionDecisionSchema composes a JSON Schema 2020-12 oneOf over the
// candidate actions (each branch discriminated by a const action_type) plus a
// no_action branch. Each action branch's payload is that action's entity.schema
// when the kit supplied one; for entity.type=existing without an override the
// payload is a permissive object — the platform's ExecuteAction validates it
// against the real ontology schema at execution time.
func buildActionDecisionSchema(candidates []agent.ActionDef) (*jsonschema.Schema, error) {
	branches := make([]any, 0, len(candidates)+1)
	for _, a := range candidates {
		var payloadSchema any = map[string]any{"type": "object"}
		if s := a.PayloadSchema(); len(s) > 0 {
			payloadSchema = s
		}
		branches = append(branches, map[string]any{
			"type":     "object",
			"required": []any{"action_type", "reasoning"},
			"properties": map[string]any{
				"action_type":      map[string]any{"const": a.Name},
				"payload":          payloadSchema,
				"reasoning":        map[string]any{"type": "string"},
				"description":      map[string]any{"type": "string"},
				"reasoning_facts":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"expected_outcome": map[string]any{"type": "string"},
			},
		})
	}
	branches = append(branches, map[string]any{
		"type":     "object",
		"required": []any{"action_type", "reasoning"},
		"properties": map[string]any{
			"action_type": map[string]any{"const": agent.ActionNoOp},
			"reasoning":   map[string]any{"type": "string"},
		},
	})

	doc := map[string]any{"oneOf": branches}
	c := jsonschema.NewCompiler()
	const url = "mem://oga/action-decision.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// buildSubmitActionInput maps the chosen action declaration + the LLM decision
// into a gateway.SubmitActionInput. Governance fields come from the action;
// routing comes from the profile (primary) and its escalation policy.
func buildSubmitActionInput(p *agent.DomainAgentProfile, action *agent.ActionDef, event *agent.ProactiveEvent, decision *agent.ActionDecision) *gateway.SubmitActionInput {
	description := decision.Description
	if description == "" {
		description = action.Description
	}
	in := &gateway.SubmitActionInput{
		ActionName:          decision.ActionType,
		Payload:             decision.Payload,
		Description:         description,
		Reasoning:           decision.Reasoning,
		ReasoningFacts:      decision.ReasoningFacts,
		ExpectedOutcome:     decision.ExpectedOutcome,
		Routing:             p.ProactiveReasoning.Routing.ToActionRouting(),
		TargetEntityID:      event.EntityID,
		TargetEventID:       event.EventID,
		TriggerEventIDs:     []string{event.EventID},
		TriggerEntityIDs:    []string{event.EntityID},
		HumanActionMode:     gateway.HumanActionMode(action.HumanActionMode),
		RiskLevel:           gateway.RiskLevel(action.RiskLevel),
		AutoApproveTimeout:  durationOrZero(action.AutoApproveTimeout),
		AutoApproveEligible: action.RiskLevel == "informational" || action.RiskLevel == "low",
	}
	if ep := p.ProactiveReasoning.EscalationPolicy; ep != nil {
		in.EscalationTimeout = durationOrZero(ep.Timeout)
		in.EscalationRouting = ep.Routing.ToActionRouting()
	}
	return in
}

func durationOrZero(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s) // already validated at profile load
	if err != nil {
		return 0
	}
	return d
}

func proactiveQuery(e *agent.ProactiveEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proactive event %s on entity %s", e.EventType, e.EntityID)
	if e.EntityType != "" {
		fmt.Fprintf(&b, " (%s)", e.EntityType)
	}
	if e.Severity != "" {
		fmt.Fprintf(&b, ", severity %s", e.Severity)
	}
	b.WriteString(". Decide what action, if any, to propose.")
	return b.String()
}

// proactiveSeedFacts renders the triggering event as a readable facts block the
// planner grounds on (OGA-419). It replaces the former {placeholder}
// substitution (OGA-350): instead of templating tool arguments, the concrete
// event facts (entity_id, type, severity, payload) are given to the LLM, which
// derives the tool arguments itself in the ReAct loop. Payload keys are sorted
// for deterministic output.
func proactiveSeedFacts(e *agent.ProactiveEvent) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Triggering event facts:\n")
	fmt.Fprintf(&b, "- event_type: %s\n", e.EventType)
	if e.EventID != "" {
		fmt.Fprintf(&b, "- event_id: %s\n", e.EventID)
	}
	if e.EntityID != "" {
		fmt.Fprintf(&b, "- entity_id: %s\n", e.EntityID)
	}
	if e.EntityType != "" {
		fmt.Fprintf(&b, "- entity_type: %s\n", e.EntityType)
	}
	if e.Severity != "" {
		fmt.Fprintf(&b, "- severity: %s\n", e.Severity)
	}
	if e.H3Cell != "" {
		fmt.Fprintf(&b, "- h3_cell: %s\n", e.H3Cell)
	}
	if len(e.Payload) > 0 {
		keys := make([]string, 0, len(e.Payload))
		for k := range e.Payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- %s: %v\n", k, e.Payload[k])
		}
	}
	return b.String()
}

func proactiveAssemblyPrompt(p *agent.DomainAgentProfile, candidates []agent.ActionDef) string {
	var b strings.Builder
	if p.ProactiveReasoning != nil && p.ProactiveReasoning.SystemPrompt != "" {
		b.WriteString(p.ProactiveReasoning.SystemPrompt)
		b.WriteString("\n\n")
	}
	b.WriteString("Based on the gathered evidence, decide whether to propose ONE of the following actions, ")
	b.WriteString("or no_action if none is warranted:\n")
	for _, a := range candidates {
		fmt.Fprintf(&b, "- %s: %s [risk=%s, mode=%s]\n", a.Name, a.Description, a.RiskLevel, a.HumanActionMode)
		// Render the action's payload schema inline so the LLM knows the
		// required fields and enum constraints it must satisfy. Without this
		// the LLM guesses payload fields and the post-hoc schema validation in
		// RunSync rejects the output (OGA-343). When PayloadSchema is nil
		// (type=existing without an override) the platform lifts the schema
		// from the active ontology at execution time, so no inline schema is
		// emitted and the LLM fills a best-effort payload.
		if schema := a.PayloadSchema(); len(schema) > 0 {
			if js, err := json.Marshal(schema); err == nil {
				fmt.Fprintf(&b, "    payload schema (JSON Schema): %s\n", js)
			}
		}
	}
	b.WriteString("- no_action: take no action\n\n")
	b.WriteString("Set action_type to your chosen action's name. ")
	b.WriteString("When the action declares a payload schema above, set payload to an object that satisfies it — ")
	b.WriteString("include EVERY required field and use ONLY allowed enum values for constrained fields. ")
	b.WriteString("Set reasoning to your justification (including why no_action, if chosen).")
	return b.String()
}
