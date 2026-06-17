package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"text/template"

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
// a free-text A2A message with intent="investigation"). It selects the planner
// per request:
//
//   - When the inbound message carries concrete investigation entity ids
//     (InvestigationContext seed ids, surfaced in the message metadata), it uses
//     InvestigationLLMPlanner: it front-loads a deterministic kg_get_entity per
//     seed entity (guaranteed grounding) and then lets the agent's full-toolbox
//     LLM planner add question-relevant evidence (SOP, history, trends) — the
//     same dynamic planner the chat path uses (OGA-378, Option 2).
//   - Otherwise (plain chat) it uses LLMToolPlanner, like the platform's
//     Knowledge Agent: the LLM plans MCP tools dynamically per request.
//
// The profile's proactive_reasoning.grounding_strategy is deliberately NOT
// consulted here. A grounding strategy is a deterministic plan tuned for an
// autonomous proactive event (it references event placeholders like
// {entity_id} that only exist on the proactive event). Running it on the
// reactive path would replay that rigid plan against an interactive query —
// the placeholders pass through literally and tool calls fail (OGA-348). The
// investigation planner above is the reactive analogue: it seeds on CONCRETE
// ids (no placeholders) and reaches the rest of the toolbox via the LLM
// planner. The grounding strategy is consumed exclusively by the proactive
// handler (NewProactiveMessageHandler → runProactiveReasoning).
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

		// Select the reactive planner per request. When the investigation forward
		// carries an enriched investigation context (OGA-381 — built server-side
		// by Frontier's Enricher), ground deterministically on its seed ids AND
		// anchor the assembly prompt to the original proposal; otherwise plan
		// tools dynamically via the LLM. The proactive grounding strategy is never
		// used here (see the doc comment + OGA-348).
		invCtx, hasInvCtx := investigationContextFromMessage(msg.Params.Message)
		var investigationIDs []string
		if hasInvCtx {
			investigationIDs = investigationSeedIDs(invCtx)
			userText = enrichQueryWithInvestigationContext(userText, invCtx)
		}
		var planner StreamPlanner
		if len(investigationIDs) > 0 {
			// Option 2 (OGA-378 rework): guarantee grounding on the proposal's
			// concrete seed entities, then let the agent's full-toolbox LLM
			// planner add question-relevant evidence (SOP, history, trends).
			planner = NewInvestigationLLMPlanner(investigationIDs, reactiveStreamPlanner(rt))
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
		// On the investigation path, append the always-on concise-briefing format
		// contract to the ASSEMBLY system prompt (not the planner input). This
		// guarantees a succinct operator-facing verdict on every investigation,
		// independent of how sparse the proposal context was. The planner builds
		// its own system prompt (RequestPlan → PlanningSystemPrompt), so this
		// never affects tool planning.
		if hasInvCtx {
			assemblyPrompt = appendInvestigationBriefingDirective(assemblyPrompt)
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

// Metadata key carrying the enriched investigation context on an inbound A2A
// message (set by Frontier when it force-routes an [Investigate] follow-up).
// Mirrors the platform's stateless-investigation contract
// (internal/agent/investigation_stateless.go) by value.
const metadataKeyInvestigationContext = "investigation_context"

// investigationContext is the local struct used to unmarshal the ENRICHED
// investigation_context JSON forwarded by Frontier's Enricher (OGA-381 §6.3).
// This is the fat, server-resolved shape — distinct from the thin wire handle
// gateway.InvestigationContextPayload (which no longer carries these fields).
// Reading it via a local struct keeps the SDK decoupled from the enriched
// shape's evolution; older payloads that lack proposal-anchoring fields degrade
// gracefully (zero-value strings are omitted from the prompt).
type investigationContext struct {
	ReasoningFacts   []string `json:"reasoning_facts"`
	TargetEntityID   string   `json:"target_entity_id"`
	TargetEventID    string   `json:"target_event_id"`
	TriggerEntityIDs []string `json:"trigger_entity_ids"`
	TriggerEventIDs  []string `json:"trigger_event_ids"`
	Description      string   `json:"description"`
	ActionType       string   `json:"action_type"`
	ExpectedOutcome  string   `json:"expected_outcome"`
	RiskLevel        string   `json:"risk_level"`
}

// investigationContextFromMessage parses the enriched investigation_context JSON
// from an inbound A2A message's metadata (OGA-381). Frontier, when it
// force-routes an [Investigate] follow-up to the proposing agent, injects the
// Enricher-built context. Returns (nil, false) when absent or unparseable — the
// caller then falls back to the LLM planner (plain chat).
func investigationContextFromMessage(m *agent.Message) (*investigationContext, bool) {
	if m == nil || m.Metadata == nil {
		return nil, false
	}
	raw, ok := m.Metadata[metadataKeyInvestigationContext]
	if !ok {
		return nil, false
	}
	s, isStr := raw.(string)
	if !isStr || s == "" {
		return nil, false
	}
	var ic investigationContext
	if err := json.Unmarshal([]byte(s), &ic); err != nil {
		return nil, false
	}
	return &ic, true
}

// investigationSeedIDs returns the deduplicated grounding seed set for the
// investigation grounding planner: the union of the singular target ids and the
// plural trigger sets. For a simple proactive proposal these collapse to one
// entity + one event; for a convergence proposal they expand to the
// system-level target plus the N correlated individuals. Falls back to the
// direct metadata array shape only when the enriched context yields nothing.
func investigationSeedIDs(ic *investigationContext) []string {
	if ic == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	add(ic.TargetEntityID)
	add(ic.TargetEventID)
	for _, id := range ic.TriggerEntityIDs {
		add(id)
	}
	for _, id := range ic.TriggerEventIDs {
		add(id)
	}
	return out
}

// investigationPromptData is the view model rendered into the assembly prompt
// by investigationContextTemplate.
type investigationPromptData struct {
	ActionType      string
	Description     string
	ExpectedOutcome string
	RiskLevel       string
	ReasoningFacts  []string
	UserQuery       string
}

// investigationContextTemplate is the proposal-anchoring block prepended to the
// user's question (OGA-381 §5.3). Sections render only when their field is
// non-empty; the trailing directive instructs the assembly LLM to brief on the
// original proposal rather than re-propose.
const investigationContextTemplate = `[Investigation context{{if .ActionType}} — the proposing agent recommended: "{{.ActionType}}"{{end}}
{{- if .Description}}
Description: {{.Description}}
{{- end}}
{{- if .ExpectedOutcome}}
Expected outcome: {{.ExpectedOutcome}}
{{- end}}
{{- if .RiskLevel}}
Risk level: {{.RiskLevel}}
{{- end}}
{{- if .ReasoningFacts}}

The proposal was raised because:
{{- range .ReasoningFacts}}
• {{.}}
{{- end}}
{{- end}}

Brief the operator on whether THIS proposal is justified, grounded ONLY in the
evidence from the tool results. Do not propose a different action.]

{{.UserQuery}}`

var investigationContextTmpl = template.Must(
	template.New("investigation_context").Parse(investigationContextTemplate),
)

// enrichQueryWithInvestigationContext prepends a proposal-anchoring block to the
// user's question so the assembly LLM briefs on THE ORIGINAL proposal rather
// than re-proposing from scratch (OGA-381 §5.3). It renders
// investigationContextTemplate with the non-empty proposal fields; when no
// anchoring fields are present (or the template fails) the query is returned
// unchanged — enrichment is best-effort and never blocks the query.
func enrichQueryWithInvestigationContext(userText string, ic *investigationContext) string {
	if ic == nil {
		return userText
	}
	facts := nonEmptyStrings(ic.ReasoningFacts)
	if ic.ActionType == "" && ic.Description == "" && ic.ExpectedOutcome == "" && len(facts) == 0 {
		return userText
	}
	data := investigationPromptData{
		ActionType:      ic.ActionType,
		Description:     ic.Description,
		ExpectedOutcome: ic.ExpectedOutcome,
		RiskLevel:       ic.RiskLevel,
		ReasoningFacts:  facts,
		UserQuery:       userText,
	}
	var b strings.Builder
	if err := investigationContextTmpl.Execute(&b, data); err != nil {
		return userText
	}
	return b.String()
}

// investigationBriefingDirective is the always-applied output-format contract
// for the reactive [Investigate] briefing. It is decoupled from
// investigationContextTemplate (which renders proposal context only when
// anchoring fields are present) so that conciseness is enforced on EVERY
// investigation briefing — even when the enriched context is sparse. It is
// appended to the ASSEMBLY system prompt (consumed only by the final assembly
// LLM call in pipeline.streamAssembly), so it constrains the operator-facing
// briefing without affecting tool planning (the planner builds its own system
// prompt via RequestPlan → PlanningSystemPrompt).
const investigationBriefingDirective = `

Keep the reply concise, succinct, and direct — at most ~200 words. Lead with a
direct answer to the operator's question (for an approve/justify question, a
one-line verdict first). Choose the format that communicates fastest — e.g. a
compact table for sensor readings, a short labelled section for a distinct topic,
brief bullets or a sentence or two otherwise. Ground every statement in the tool
results; do not speculate about evidence you were not given, and say so plainly
when it is insufficient.`

// appendInvestigationBriefingDirective appends the always-on briefing format
// contract to the assembly system prompt. Applied on every reactive
// investigation regardless of how sparse the proposal context is, so the final
// briefing is concise even when investigationContextTemplate rendered nothing.
func appendInvestigationBriefingDirective(systemPrompt string) string {
	return systemPrompt + investigationBriefingDirective
}

// nonEmptyStrings returns a copy of in with empty elements dropped.
func nonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
