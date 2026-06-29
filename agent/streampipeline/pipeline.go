package streampipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// Config holds operational tunables for Pipeline.Run.
type Config struct {
	// ToolTimeout caps each per-step MCP call. Default 30s.
	ToolTimeout time.Duration

	// AssemblyTimeout caps the final LLM assembly call. Default 60s.
	AssemblyTimeout time.Duration

	// MaxCitationsPerStep caps entity citations per tool result.
	// Default 10 (matches platform Knowledge Agent).
	MaxCitationsPerStep int

	// MaxSteps caps total execution-plan size. Default 10.
	MaxSteps int

	// MaxResultPreviewBytes caps the inline preview in task/tool_result events.
	// Default 2048.
	MaxResultPreviewBytes int

	// MaxArtifactSummaryBytes caps task/tool_result.summary length.
	// Default 200.
	MaxArtifactSummaryBytes int

	// NoProgressLimit stops the ReAct loop early when the planner repeats an
	// identical (tool, args) action or returns K consecutive empty/failed
	// observations (OGA-419, Property 3). Default 2.
	NoProgressLimit int
}

// DefaultConfig returns conservative production defaults.
func DefaultConfig() Config {
	return Config{
		ToolTimeout:             30 * time.Second,
		AssemblyTimeout:         60 * time.Second,
		MaxCitationsPerStep:     MaxCitationsPerStep,
		MaxSteps:                10,
		MaxResultPreviewBytes:   2048,
		MaxArtifactSummaryBytes: 200,
		NoProgressLimit:         2,
	}
}

// Deps wires platform services into the pipeline. The Gateway is the single
// access point — all MCP tool calls, LLM completions, and (reactive) agent
// delegation go through it for uniform PBAC, audit, rate limiting, and tenant
// attribution (OGA-303). Typed as the PlatformAccess interface (OGA-419) so the
// platform Knowledge Agent can supply an adapter to its own MCP + LLM endpoints
// instead of the Platform Gateway client.
type Deps struct {
	Gateway PlatformAccess
	Logger  *slog.Logger
	Config  Config
}

// Input is the per-request configuration the pipeline needs from its caller.
type Input struct {
	// Query is the user's message text. Used for the final LLM ASSEMBLY call
	// (the briefing). On the investigation path this carries the proposal
	// anchoring + the "ground ONLY in the tool results / do not re-propose"
	// directive so the briefing judges the original proposal correctly.
	Query string

	// PlannerQuery, when non-empty, is the text handed to the StreamPlanner
	// instead of Query (OGA-398). It carries a PLANNING framing (what evidence
	// does answering this question require?) WITHOUT the assembly-only
	// constraints that live in Query ("ground ONLY in results", "do not
	// re-propose") — those instructions, fed to a planner, suppress evidence
	// gathering and produce an empty plan. Empty → the planner uses Query
	// (plain chat / non-investigation paths are unaffected).
	PlannerQuery string

	// TenantID identifies the tenant. Embedded in events for observability.
	TenantID string

	// PrincipalID identifies the user. Embedded in events for observability.
	PrincipalID string

	// Actor describes who emits events ("knowledge-agent", "fm-operations-agent", etc.).
	Actor agent.EventActor

	// AssemblyPrompt is the system prompt used for the final LLM assembly call.
	// Knowledge Agent passes its built-in assembly prompt; domain agents pass
	// their persona prompt with the locale + interaction-style overlay applied.
	AssemblyPrompt string

	// AssemblyModel optionally selects the model for the terminal assembly call
	// independently of the per-turn decision model (OGA-420 Gap 1). Empty → the
	// gateway default route (today's behavior). The decision model is configured
	// separately via the planner's PlannerConfig.
	AssemblyModel string

	// AssemblyMaxTokens caps the assembly response. 0 → the pipeline default
	// (2048), preserving pre-OGA-420 behavior when unset.
	AssemblyMaxTokens int

	// AssemblyTemperature overrides the assembly sampling temperature. Nil →
	// gateway default. Pointer so 0.0 is distinguishable from unset.
	AssemblyTemperature *float64

	// ToolNames is the union of MCP tool names available to this agent. Passed
	// to the StreamPlanner. May be empty if the planner doesn't need it.
	ToolNames []string

	// InvestigationEntityIDs are the concrete KG entity ids a reactive
	// investigation should ground on (OGA-378). When non-empty, the handler
	// selects the deterministic InvestigationGroundingPlanner (seed retrieval
	// from these ids) instead of the LLMToolPlanner. Empty on plain chat.
	// These are concrete ids carried on the investigation forward — NOT the
	// proactive {entity_id} placeholders (which only exist on the proactive
	// path).
	InvestigationEntityIDs []string

	// Persona is the system prompt + tool palette handed to the planner each
	// turn (OGA-419). For a domain agent it is built from the profile; for the
	// Knowledge Agent it is built from its planner prompt + kg_* tools. The
	// Tools slice bounds what the planner may call (the palette guardrail — e.g.
	// the proactive palette excludes any agent-delegation capability).
	Persona PlannerPersona

	// GroundingStrategy is the kit-declared grounding strategy surfaced to the
	// planner as ADVISORY hints (OGA-419). Populated when the agent has a
	// profile strategy (domain agents, both proactive and reactive paths);
	// empty for profile-less platform agents (the Knowledge Agent).
	GroundingStrategy []agent.GroundingStep

	// SeedFacts is resolved factual context the planner grounds on without
	// re-deriving it: proactive event facts, or the reactive investigation
	// context. Empty for plain reactive chat.
	SeedFacts string

	// Delegations declares agent-delegation capabilities available on this path
	// (OGA-419 G3). Each entry maps a palette tool name (e.g. "ask_knowledge_agent")
	// to a downstream agent the loop invokes via PlatformAccess.InvokeAgentStream
	// — streaming its events nested under a task/agent_call span — instead of
	// PlatformAccess.CallTool. Reactive-only: the proactive proposal path leaves
	// this empty so a second non-deterministic reasoner can never sit inside the
	// proposal evidence chain (Property 5).
	Delegations []AgentDelegation
}

// AgentDelegation maps a planner palette tool name to a downstream agent the
// pipeline invokes via streaming delegation (OGA-419 G3). The ToolName appears
// in the planner's palette; when the planner selects it, the loop emits
// task/agent_call, streams the sub-agent's events re-parented under that call,
// emits task/agent_result, and appends the sub-agent's final answer as the
// turn's observation.
type AgentDelegation struct {
	// ToolName is the palette name the planner selects to trigger delegation
	// (e.g. "ask_knowledge_agent").
	ToolName string
	// AgentName is the downstream agent's gateway name (e.g. "knowledge-agent").
	AgentName string
	// Description is rendered into the decision prompt so the planner knows when
	// to use the delegation.
	Description string
}

// Pipeline is the shared streaming orchestrator. Construct with NewPipeline
// and reuse across requests — Pipeline.Run is goroutine-safe.
type Pipeline struct{}

// NewPipeline returns a fresh pipeline.
func NewPipeline() *Pipeline { return &Pipeline{} }

// Run executes the canonical streaming sequence:
//
//	task/reasoning (planner narrative)
//	  → task/plan
//	  → for each step: task/tool_call, task/tool_result, task/citation
//	                   (or task/tool_call{Skipped} when conditional skip)
//	  → task/reasoning ("Assembling response...")
//	  → token-streamed task/artifact
//	  → consolidated task/citation
//	  → task/status{completed}
//
// On error, emits task/status{failed} and returns the error. The caller is
// responsible for closing the events channel after Run returns.
//
// Run is safe to invoke concurrently; events from different invocations carry
// distinct task IDs.
func (p *Pipeline) Run(
	ctx context.Context,
	deps Deps,
	input Input,
	planner Planner,
	events chan<- *agent.StreamEvent,
) error {
	return p.runInternal(ctx, deps, input, planner, events, deps.Gateway)
}

// runInternal is the test seam: it accepts the PlatformAccess interface
// directly so tests can inject a fake without constructing a real
// *gateway.PlatformGatewayClient. Production callers go through Run.
func (p *Pipeline) runInternal(
	ctx context.Context,
	deps Deps,
	input Input,
	planner Planner,
	events chan<- *agent.StreamEvent,
	gw PlatformAccess,
) error {
	cfg := deps.Config
	if cfg.ToolTimeout == 0 {
		cfg = DefaultConfig()
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultConfig().MaxSteps
	}
	if cfg.NoProgressLimit <= 0 {
		cfg.NoProgressLimit = DefaultConfig().NoProgressLimit
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tracker := agent.NewSpanTracker("")
	rootSpan := tracker.RootSpan()
	emitter := newEmitter(events, uuid.New().String(), input.Actor)

	// ReAct loop (OGA-419): gather evidence one action per turn. Extracted into
	// reactLoop so the synchronous RunSync path (gatherSync) reuses the exact
	// same engine, and a schema-validation retry re-assembles WITHOUT re-running
	// tools (OGA-423 Gap 2A).
	g := p.reactLoop(ctx, deps, input, planner, cfg, logger, tracker, rootSpan, emitter, gw)
	if g.terminal != nil {
		return g.terminal // reactLoop already emitted the terminal status event
	}
	if g.plainAnswer {
		return p.runPlainAnswer(ctx, gw, input, cfg, tracker, rootSpan, emitter, g.plainReason)
	}

	// Assembly.
	emitter.emitReasoning(tracker.ChildSpan(rootSpan), rootSpan, 1, "Assembling response...", false)
	asmUsage, asmAvail, err := p.streamAssembly(ctx, gw, input, cfg, tracker, rootSpan, emitter, g.results)
	if err != nil {
		emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
			Code:    -32000,
			Message: "assembly failed: " + err.Error(),
		})
		return fmt.Errorf("assembly: %w", err)
	}
	aggUsage, aggUsageAvail := g.decisionUsage, g.usageAvail
	if asmAvail {
		aggUsage = aggUsage.Add(asmUsage)
		aggUsageAvail = true
	}

	// Consolidated citation.
	if len(g.citations) > 0 {
		emitter.emitCitation(rootSpan, g.citations)
	}

	// Per-request token-usage aggregate (OGA-420).
	emitter.emitUsage(tracker.ChildSpan(rootSpan), rootSpan, agent.UsageRoleAggregate, -1, input.AssemblyModel, aggUsage, aggUsageAvail)

	// Final status.
	emitter.emitStatus(rootSpan, agent.TaskStateCompleted, nil)
	return nil
}

// gatherResult is the output of the ReAct evidence loop (reactLoop), shared by
// the streaming entry point (runInternal) and the synchronous entry point
// (gatherSync). It carries the gathered transcript + consolidated citations +
// per-turn decision usage, plus two control signals: plainAnswer (first-turn
// planning failed with no evidence → caller answers directly from the model) and
// terminal (the loop already emitted a failed/canceled status and the caller
// must return this error without assembling).
type gatherResult struct {
	results       []ToolStepResult
	citations     []agent.CitationSource
	decisionUsage agent.TokenUsage
	usageAvail    bool
	plainAnswer   bool
	plainReason   string
	terminal      error
}

// reactLoop runs the ReAct evidence loop: decide ONE action per turn against the
// full observation transcript, execute it, observe, repeat — until the planner
// finalizes or the step budget / no-progress guard stops it (OGA-419). It emits
// every per-turn event (reasoning, plan, tool_call, tool_result, citation,
// usage) to the supplied emitter and returns the gathered transcript. It does
// NOT perform the final assembly — the caller owns that, which lets RunSync
// re-assemble on a schema-validation failure without re-running tools (OGA-423).
func (p *Pipeline) reactLoop(
	ctx context.Context,
	deps Deps,
	input Input,
	planner Planner,
	cfg Config,
	logger *slog.Logger,
	tracker *agent.SpanTracker,
	rootSpan string,
	emitter *eventEmitter,
	gw PlatformAccess,
) gatherResult {
	plannerQuery := input.Query
	if strings.TrimSpace(input.PlannerQuery) != "" {
		plannerQuery = input.PlannerQuery
	}

	results := make([]ToolStepResult, 0, cfg.MaxSteps)
	allCitations := make([]agent.CitationSource, 0)
	decided := make([]ToolPlanStep, 0, cfg.MaxSteps) // for the evolving task/plan

	// Per-request decision token-usage aggregate (OGA-420): summed across every
	// per-turn decision call. The caller folds in the assembly usage.
	var aggUsage agent.TokenUsage
	var aggUsageAvail bool

	// Delegation palette: palette tool names that trigger streaming agent
	// delegation instead of a tool call (OGA-419 G3). Empty on the proactive
	// path (Property 5).
	delegations := make(map[string]AgentDelegation, len(input.Delegations))
	for _, d := range input.Delegations {
		if d.ToolName != "" && d.AgentName != "" {
			delegations[d.ToolName] = d
		}
	}

	// Discovered tool schemas, indexed by name, for the pre-dispatch
	// required/enum check in executeStep (OGA-438).
	toolSchemas := make(map[string]agent.ToolSchema, len(input.Persona.ToolSchemas))
	for _, s := range input.Persona.ToolSchemas {
		if s.Name != "" {
			toolSchemas[s.Name] = s
		}
	}

	for turn := 0; turn < cfg.MaxSteps; turn++ {
		st := &PlanState{
			Query:             plannerQuery,
			Persona:           input.Persona,
			GroundingStrategy: input.GroundingStrategy,
			SeedFacts:         input.SeedFacts,
			History:           results,
			StepBudget:        cfg.MaxSteps - turn,
		}

		decision, err := planner.Next(ctx, st)
		if err != nil {
			// Context cancellation during planning is terminal (OGA-368): no
			// fallback against a dead context.
			if ctx.Err() != nil {
				emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
					Code:    -32000,
					Message: "planning failed: " + err.Error(),
				})
				return gatherResult{terminal: fmt.Errorf("planner.Next: %w", err)}
			}
			// No observations yet → degrade to a plain LLM answer (mirrors the
			// pre-OGA-419 fallback). The caller answers directly from the model;
			// if the gateway is genuinely down its assembly call fails and
			// surfaces task/status{failed}, so a real transport failure is never
			// masked.
			if len(results) == 0 {
				logger.WarnContext(ctx, "streampipeline: first-turn planning failed, falling back to plain answer",
					"error", err)
				return gatherResult{
					plainAnswer: true,
					plainReason: "Tool planning was unavailable; answering directly from the model.",
				}
			}
			// Mid-loop failure with evidence already gathered → stop gathering
			// and assemble honestly from what we have (Property 4 — never
			// fabricate; the assembly grounds only on real observations).
			logger.WarnContext(ctx, "streampipeline: mid-loop planning failed; assembling with evidence so far",
				"error", err, "observations", len(results))
			break
		}

		// Emit the "Thought".
		if decision.Narrative != "" {
			emitter.emitReasoning(tracker.ChildSpan(rootSpan), rootSpan, 1, decision.Narrative, false)
		}

		// Per-turn decision token usage (OGA-420), folded into the per-request
		// aggregate. Emitted as its own event only when the proxy reported usage.
		if decision.UsageAvailable {
			aggUsage = aggUsage.Add(decision.Usage)
			aggUsageAvail = true
			emitter.emitUsage(tracker.ChildSpan(rootSpan), rootSpan, agent.UsageRoleDecision, turn, "", decision.Usage, true)
		}

		// Planner finalized → go to assembly.
		if decision.Done || decision.Step == nil {
			break
		}

		// Mid-loop cancellation (operator abort after a successful decision) →
		// canceled, distinct from a planning-time failure above.
		if ctx.Err() != nil {
			emitter.emitStatus(rootSpan, agent.TaskStateCanceled, &agent.StatusError{
				Code:    -32000,
				Message: "cancelled: " + ctx.Err().Error(),
			})
			return gatherResult{terminal: ctx.Err()}
		}

		step := *decision.Step
		idx := len(results)

		// No-progress guard (Property 3): identical (tool,args) repeat, or K
		// consecutive empty/failed observations.
		if noProgress(decided, results, step, cfg.NoProgressLimit) {
			logger.WarnContext(ctx, "streampipeline: no-progress detected, stopping loop",
				"tool", step.ToolName, "turn", turn)
			break
		}

		// Evolving plan: append the decided step and re-emit the cumulative
		// plan so the UI checklist grows turn by turn (OGAW merges by index).
		decided = append(decided, step)
		emitter.emitPlan(rootSpan, &ToolPlan{Steps: decided})

		toolSpan := tracker.ChildSpan(rootSpan)

		// Delegation: the planner selected an agent-delegation capability. Stream
		// the sub-agent under this turn (re-parented) instead of calling an MCP
		// tool (OGA-419 G3). The sub-agent's final answer becomes this turn's
		// observation so the loop reasons over it next turn.
		if d, ok := delegations[step.ToolName]; ok {
			result, cites := p.runDelegation(ctx, gw, d, step, idx, input, emitter, toolSpan, rootSpan, cfg)
			results = append(results, result)
			allCitations = append(allCitations, cites...)
			continue
		}

		// Condition is advisory under ReAct (LLM steps carry none); honored for
		// precomputed seed/grounding steps that set it.
		shouldRun, skipReason := evaluateCondition(step.Condition)
		if !shouldRun {
			emitter.emitToolCallSkipped(toolSpan, rootSpan, step, idx, skipReason)
			results = append(results, ToolStepResult{
				StepIndex:  idx,
				ToolName:   step.ToolName,
				Skipped:    true,
				SkipReason: skipReason,
			})
			continue
		}

		emitter.emitToolCall(toolSpan, rootSpan, step, idx)

		result := executeStep(ctx, gw, step, idx, results, cfg.ToolTimeout, toolSchemas)
		emitter.emitToolResult(toolSpan, rootSpan, &result, cfg)

		// Required-step failure → fail-fast (honored for grounding/seed steps).
		if step.Required && !result.Success && !result.Skipped {
			emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
				Code:    -32000,
				Message: fmt.Sprintf("required step %q failed: %s", step.Name, result.Error),
			})
			return gatherResult{terminal: fmt.Errorf("required step %q failed: %s", step.Name, result.Error)}
		}

		results = append(results, result)

		// Extract + emit citations.
		citations := ExtractCitations(&result, step.ToolName, step.Arguments)
		if len(citations) > 0 {
			emitter.emitCitation(toolSpan, citations)
			allCitations = append(allCitations, citations...)
		}
	}

	return gatherResult{
		results:       results,
		citations:     allCitations,
		decisionUsage: aggUsage,
		usageAvail:    aggUsageAvail,
	}
}

// runPlainAnswer is invoked when the planner returns 0 steps (e.g., trivial
// greeting, or LLM judges no tools needed) or when planning failed and the
// pipeline degrades to an ungrounded answer (per OGA-368). Streams a single
// LLM response as task/artifact, no plan / tool / citation events. The
// reasoningText is emitted as the leading task/reasoning event so the operator
// sees why no tools were used.
func (p *Pipeline) runPlainAnswer(
	ctx context.Context,
	gw PlatformAccess,
	input Input,
	cfg Config,
	tracker *agent.SpanTracker,
	rootSpan string,
	emitter *eventEmitter,
	reasoningText string,
) error {
	emitter.emitReasoning(tracker.ChildSpan(rootSpan), rootSpan, 1, reasoningText, false)

	asmUsage, asmAvail, err := p.streamAssembly(ctx, gw, input, cfg, tracker, rootSpan, emitter, nil)
	if err != nil {
		emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
			Code:    -32000,
			Message: "assembly failed: " + err.Error(),
		})
		return err
	}
	// Per-request aggregate (OGA-420): plain-answer has no decision turns, so the
	// aggregate is just the assembly usage.
	emitter.emitUsage(tracker.ChildSpan(rootSpan), rootSpan, agent.UsageRoleAggregate, -1, input.AssemblyModel, asmUsage, asmAvail)
	emitter.emitStatus(rootSpan, agent.TaskStateCompleted, nil)
	return nil
}

// streamAssembly builds the assembly prompt from prior tool results and
// streams the LLM response as task/artifact events. Falls back to a single
// chat completion when streaming is unavailable. It applies the per-request
// assembly model selection (OGA-420 Gap 1) and captures the call's token usage
// (OGA-420 Gap 2): on the streaming path via stream_options.include_usage (a
// final usage-bearing chunk), on the sync fallback via the response Usage. It
// emits a task/usage{assembly} event and returns the usage so the caller folds
// it into the per-request aggregate. The bool is false when the proxy reported
// no usage (counts then zero, must not be read as a real "0 tokens").
func (p *Pipeline) streamAssembly(
	ctx context.Context,
	gw PlatformAccess,
	input Input,
	cfg Config,
	tracker *agent.SpanTracker,
	rootSpan string,
	emitter *eventEmitter,
	results []ToolStepResult,
) (agent.TokenUsage, bool, error) {
	systemPrompt := input.AssemblyPrompt
	if systemPrompt == "" {
		systemPrompt = "You are a helpful agent. Answer the user's question using the provided context."
	}

	var resultCtx strings.Builder
	for i, r := range results {
		if r.Skipped {
			continue
		}
		if r.Success {
			fmt.Fprintf(&resultCtx, "Tool %d: %s\nResult:\n%s\n\n", i+1, r.ToolName, r.Content)
		} else {
			fmt.Fprintf(&resultCtx, "Tool %d: %s\nError: %s\n\n", i+1, r.ToolName, r.Error)
		}
	}

	userPrompt := input.Query
	if resultCtx.Len() > 0 {
		userPrompt = "Original user question: " + input.Query + "\n\nTool results:\n" + resultCtx.String()
	}

	// Assembly model selection (OGA-420 Gap 1). Empty Model / zero MaxTokens
	// collapse to the prior behavior (gateway default route, 2048 tokens).
	maxTokens := input.AssemblyMaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	req := &gateway.ChatCompletionRequest{
		Model: input.AssemblyModel,
		Messages: []gateway.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens:   maxTokens,
		Temperature: input.AssemblyTemperature,
	}

	// Trace: the effective reasoning prompt actually sent to the assembly LLM.
	// This is the fully-composed system prompt (kit persona + locale overlay +
	// the JSON-only / schema instruction RunSync appends) and the user prompt
	// (the query plus the gathered tool-result context). Gated by OGA_AGENT_TRACE.
	if traceEnabled() {
		const promptCap = 8192
		slog.InfoContext(ctx, "trace: effective reasoning prompt",
			"actor", input.Actor.ID,
			"tenant_id", input.TenantID,
			"tool_results", len(results),
			"system_prompt", truncateForTrace(systemPrompt, promptCap),
			"user_prompt", truncateForTrace(userPrompt, promptCap),
		)
	}

	asmCtx, cancel := context.WithTimeout(ctx, cfg.AssemblyTimeout)
	defer cancel()

	asmSpan := tracker.ChildSpan(rootSpan)

	var usage agent.TokenUsage
	var usageAvail bool

	// Try streaming first. Fall back to non-streaming on error.
	if gw != nil {
		req.Stream = true
		req.StreamOptions = &gateway.StreamOptions{IncludeUsage: true}
		tokenCh, streamErr := gw.ChatCompletionStream(asmCtx, req)
		if streamErr == nil && tokenCh != nil {
			first := true
			anyContent := false
			chunkCount := 0
			artifactBytes := 0
			for chunk := range tokenCh {
				if asmCtx.Err() != nil {
					break
				}
				// Capture usage from the final usage-bearing chunk (OGA-420).
				if tu, ok := agent.UsageFromGateway(chunk.Usage); ok {
					usage, usageAvail = tu, true
				}
				for _, choice := range chunk.Choices {
					if choice.Delta.Content == "" {
						continue
					}
					emitter.emitArtifact(asmSpan, choice.Delta.Content, !first)
					first = false
					anyContent = true
					chunkCount++
					artifactBytes += len(choice.Delta.Content)
				}
			}
			if anyContent {
				if traceEnabled() {
					slog.InfoContext(ctx, "trace: stream-collect complete",
						"actor", input.Actor.ID,
						"tenant_id", input.TenantID,
						"mode", "stream",
						"chunks", chunkCount,
						"artifact_bytes", artifactBytes,
					)
				}
				if usageAvail {
					emitter.emitUsage(asmSpan, rootSpan, agent.UsageRoleAssembly, -1, req.Model, usage, true)
				}
				return usage, usageAvail, nil
			}
			// Stream produced nothing — fall through to sync.
			req.StreamOptions = nil
		}
	}

	// Non-streaming fallback.
	req.Stream = false
	resp, err := gw.ChatCompletion(asmCtx, req)
	if err != nil {
		return agent.TokenUsage{}, false, err
	}
	if len(resp.Choices) == 0 {
		return agent.TokenUsage{}, false, errors.New("no choices in assembly response")
	}
	if tu, ok := agent.UsageFromGateway(resp.Usage); ok {
		usage, usageAvail = tu, true
	}
	answer := strings.TrimSpace(resp.Choices[0].Message.Content)
	if traceEnabled() {
		slog.InfoContext(ctx, "trace: stream-collect complete",
			"actor", input.Actor.ID,
			"tenant_id", input.TenantID,
			"mode", "sync_fallback",
			"chunks", 1,
			"artifact_bytes", len(answer),
		)
	}
	emitter.emitArtifact(asmSpan, answer, false)
	if usageAvail {
		emitter.emitUsage(asmSpan, rootSpan, agent.UsageRoleAssembly, -1, req.Model, usage, true)
	}
	return usage, usageAvail, nil
}

// runArtifact drives the pipeline but drains all events to a buffer and
// returns the assembled artifact text + consolidated citations + the
// per-request token-usage aggregate (OGA-420). It backs the typed RunSync[T]
// (see runsync.go) and any non-streaming message/send path.
//
// While draining it emits structured per-event logs for the proactive /
// stream→collect ReAct loop (OGA-420 Gap 3) when proactiveReActLogEnabled() is
// set — the actual drained events (Thought, plan, tool_call, tool_result,
// usage, terminal status), never a synthesized trace — so a proactive proposal
// that is wrong, thin, or never fires can be reconstructed from logs. Off by
// default to avoid steady-state log spam.
func (p *Pipeline) runArtifact(
	ctx context.Context,
	deps Deps,
	input Input,
	planner Planner,
) (string, []agent.CitationSource, agent.TokenUsage, bool, error) {
	events := make(chan *agent.StreamEvent, 64)

	type result struct {
		artifact   strings.Builder
		citations  []agent.CitationSource
		usage      agent.TokenUsage
		usageAvail bool
	}
	final := &result{}
	done := make(chan error, 1)

	go func() {
		err := p.Run(ctx, deps, input, planner, events)
		close(events)
		done <- err
	}()

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Per-turn ReAct logging fires under either the dedicated proactive flag or
	// the general agent-trace flag (OGA-423 Gap 1) — so OGA_AGENT_TRACE alone
	// surfaces the turn-by-turn reasoning + decisions, not just the assembly
	// prompt.
	reactLog := proactiveReActLogEnabled() || traceEnabled()

	for evt := range events {
		switch evt.Type {
		case agent.EventTypeArtifact:
			if payload, ok := evt.Payload.(*agent.ArtifactPayload); ok {
				for _, part := range payload.Parts {
					final.artifact.WriteString(part.Text)
				}
			}
		case agent.EventTypeCitation:
			if payload, ok := evt.Payload.(*agent.CitationPayload); ok {
				// Last citation event = consolidated; just keep the latest.
				final.citations = payload.Sources
			}
		case agent.EventTypeUsage:
			if payload, ok := evt.Payload.(*agent.UsagePayload); ok && payload.Role == agent.UsageRoleAggregate {
				final.usage, final.usageAvail = payload.Usage, payload.Available
			}
		}
		if reactLog {
			logDrainedEvent(ctx, logger, input, evt, deps.Config)
		}
	}

	err := <-done
	if traceEnabled() {
		logger.InfoContext(ctx, "trace: artifact assembled (stream-collected)",
			"actor", input.Actor.ID,
			"tenant_id", input.TenantID,
			"artifact_bytes", final.artifact.Len(),
			"citations", len(final.citations),
		)
	}
	return final.artifact.String(), final.citations, final.usage, final.usageAvail, err
}

// gatherSync runs the ReAct evidence loop buffered (no final assembly), draining
// the event stream for optional per-turn react logging, and returns the gathered
// transcript + consolidated citations + per-turn decision usage. It backs
// RunSync's gather-once / assemble-twice contract so a schema-validation retry
// re-assembles WITHOUT re-running tools (OGA-423 Gap 2A).
func (p *Pipeline) gatherSync(
	ctx context.Context,
	deps Deps,
	input Input,
	planner Planner,
) ([]ToolStepResult, []agent.CitationSource, agent.TokenUsage, bool, error) {
	cfg := deps.Config
	if cfg.ToolTimeout == 0 {
		cfg = DefaultConfig()
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultConfig().MaxSteps
	}
	if cfg.NoProgressLimit <= 0 {
		cfg.NoProgressLimit = DefaultConfig().NoProgressLimit
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tracker := agent.NewSpanTracker("")
	rootSpan := tracker.RootSpan()
	events := make(chan *agent.StreamEvent, 64)
	emitter := newEmitter(events, uuid.New().String(), input.Actor)

	var g gatherResult
	done := make(chan struct{})
	go func() {
		g = p.reactLoop(ctx, deps, input, planner, cfg, logger, tracker, rootSpan, emitter, deps.Gateway)
		close(events)
		close(done)
	}()

	reactLog := proactiveReActLogEnabled() || traceEnabled()
	for evt := range events {
		if reactLog {
			logDrainedEvent(ctx, logger, input, evt, cfg)
		}
	}
	<-done

	if g.terminal != nil {
		return nil, nil, agent.TokenUsage{}, false, g.terminal
	}
	return g.results, g.citations, g.decisionUsage, g.usageAvail, nil
}

// assembleSync runs ONLY the final assembly LLM call against a PRE-GATHERED
// transcript and returns the assembled artifact text + its token usage. It does
// NOT run the ReAct loop or execute any tool — RunSync uses it to retry a failed
// schema validation without re-executing tools (OGA-423 Gap 2A).
func (p *Pipeline) assembleSync(
	ctx context.Context,
	deps Deps,
	input Input,
	results []ToolStepResult,
) (string, agent.TokenUsage, bool, error) {
	cfg := deps.Config
	if cfg.ToolTimeout == 0 {
		cfg = DefaultConfig()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tracker := agent.NewSpanTracker("")
	rootSpan := tracker.RootSpan()
	events := make(chan *agent.StreamEvent, 64)
	emitter := newEmitter(events, uuid.New().String(), input.Actor)

	reactLog := proactiveReActLogEnabled() || traceEnabled()

	type asmOut struct {
		usage agent.TokenUsage
		avail bool
		err   error
	}
	res := make(chan asmOut, 1)
	go func() {
		u, a, err := p.streamAssembly(ctx, deps.Gateway, input, cfg, tracker, rootSpan, emitter, results)
		close(events)
		res <- asmOut{usage: u, avail: a, err: err}
	}()

	var sb strings.Builder
	for evt := range events {
		if evt.Type == agent.EventTypeArtifact {
			if pl, ok := evt.Payload.(*agent.ArtifactPayload); ok {
				for _, part := range pl.Parts {
					sb.WriteString(part.Text)
				}
			}
		}
		if reactLog {
			logDrainedEvent(ctx, logger, input, evt, cfg)
		}
	}
	o := <-res
	return sb.String(), o.usage, o.avail, o.err
}

// logDrainedEvent emits one structured log line for a drained StreamEvent on
// the proactive / stream→collect path (OGA-420 Gap 3). It logs the ACTUAL event
// — never a synthesized trace — bounding large tool-result previews so a verbose
// observation can't flood the log. Common fields (turn/span) make the loop
// reconstructable from logs.
func logDrainedEvent(ctx context.Context, logger *slog.Logger, input Input, evt *agent.StreamEvent, cfg Config) {
	if evt == nil {
		return
	}
	base := []any{
		"actor", input.Actor.ID,
		"tenant_id", input.TenantID,
		"seq", evt.Sequence,
		"span_id", evt.SpanID,
		"parent_span_id", evt.ParentSpanID,
		"event", string(evt.Type),
	}
	previewCap := cfg.MaxResultPreviewBytes
	if previewCap <= 0 {
		previewCap = DefaultConfig().MaxResultPreviewBytes
	}

	switch evt.Type {
	case agent.EventTypeReasoning:
		if p, ok := evt.Payload.(*agent.ReasoningPayload); ok {
			logger.InfoContext(ctx, "react: thought", append(base, "text", truncateForTrace(p.Text, previewCap))...)
		}
	case agent.EventTypePlan:
		if p, ok := evt.Payload.(*agent.PlanPayload); ok {
			tools := make([]string, 0, len(p.Steps))
			for _, s := range p.Steps {
				tools = append(tools, s.Tool)
			}
			logger.InfoContext(ctx, "react: plan", append(base, "steps", len(p.Steps), "tools", strings.Join(tools, ","))...)
		}
	case agent.EventTypeToolCall:
		if p, ok := evt.Payload.(*agent.ToolCallPayload); ok {
			args := ""
			if b, err := json.Marshal(p.Arguments); err == nil {
				args = truncateForTrace(string(b), previewCap)
			}
			logger.InfoContext(ctx, "react: action", append(base,
				"tool", p.ToolName, "step_index", p.StepIndex, "skipped", p.Skipped, "args", args)...)
		}
	case agent.EventTypeToolResult:
		if p, ok := evt.Payload.(*agent.ToolResultPayload); ok {
			outcome := "ok"
			switch {
			case !p.Success:
				outcome = "failed"
			case p.ResultSizeBytes == 0:
				outcome = "empty"
			}
			logger.InfoContext(ctx, "react: observation", append(base,
				"tool", p.ToolName, "step_index", p.StepIndex, "outcome", outcome,
				"result_bytes", p.ResultSizeBytes, "latency_ms", p.LatencyMs,
				"error_code", p.ErrorCode, "preview", truncateForTrace(p.ResultPreview, previewCap))...)
		}
	case agent.EventTypeUsage:
		if p, ok := evt.Payload.(*agent.UsagePayload); ok {
			logger.InfoContext(ctx, "react: usage", append(base,
				"role", p.Role, "turn", p.TurnIndex, "model", p.Model, "available", p.Available,
				"prompt_tokens", p.Usage.PromptTokens, "completion_tokens", p.Usage.CompletionTokens,
				"total_tokens", p.Usage.TotalTokens)...)
		}
	case agent.EventTypeStatus:
		if p, ok := evt.Payload.(*agent.StatusPayload); ok {
			if p.Error != nil {
				logger.InfoContext(ctx, "react: status", append(base, "state", p.State, "error", p.Error.Message)...)
			} else {
				logger.InfoContext(ctx, "react: status", append(base, "state", p.State)...)
			}
		}
	case agent.EventTypeAgentCall, agent.EventTypeAgentResult, agent.EventTypeCitation:
		logger.InfoContext(ctx, "react: event", base...)
	}
}

// runDelegation streams a sub-agent under the current turn (OGA-419 G3). It
// emits task/agent_call, invokes the sub-agent via PlatformAccess.InvokeAgentStream,
// re-parents and forwards the sub-agent's events nested under the call span,
// accumulates the sub-agent's final artifact text + citations, emits
// task/agent_result, and returns the artifact as this turn's observation so the
// planner reasons over it next turn. The sub-agent's own terminal task/status
// events are NOT forwarded — task/agent_result is the completion signal for the
// delegation. Bounded by the per-step ToolTimeout; one hop (the sub-agent is a
// leaf and must not call back).
func (p *Pipeline) runDelegation(
	ctx context.Context,
	gw PlatformAccess,
	d AgentDelegation,
	step ToolPlanStep,
	idx int,
	input Input,
	emitter *eventEmitter,
	callSpan, rootSpan string,
	cfg Config,
) (ToolStepResult, []agent.CitationSource) {
	subQuery := delegationQuery(step, input.Query)
	actor := agent.EventActor{Type: "sub_agent", ID: d.AgentName, DisplayName: d.AgentName}

	emitter.emitAgentCall(callSpan, rootSpan, actor, &agent.AgentCallPayload{
		TargetAgent:       d.AgentName,
		AgentType:         "platform",
		Task:              subQuery,
		SupportsStreaming: true,
	})

	res := ToolStepResult{StepIndex: idx, ToolName: d.ToolName}

	delCtx, cancel := context.WithTimeout(ctx, cfg.ToolTimeout)
	defer cancel()

	start := time.Now()
	msg := map[string]any{"role": "user", "parts": []map[string]any{{"text": subQuery}}}
	stream, err := gw.InvokeAgentStream(delCtx, d.AgentName, msg)
	if err != nil {
		res.Success = false
		res.Error = fmt.Sprintf("delegate to %s: %v", d.AgentName, err)
		res.LatencyMs = time.Since(start).Milliseconds()
		emitter.emitAgentResult(callSpan, rootSpan, actor, &agent.AgentResultPayload{
			AgentID: d.AgentName, Success: false, Error: res.Error, LatencyMs: res.LatencyMs,
		})
		return res, nil
	}

	var (
		answer    strings.Builder
		citations []agent.CitationSource
		subFailed string
	)
	for raw := range stream {
		if raw == nil {
			continue
		}
		evt := decodeDelegatedEvent(*raw)
		if evt == nil {
			continue
		}
		switch evt.Type {
		case agent.EventTypeArtifact:
			answer.WriteString(artifactText(evt.Payload))
			emitter.emitForwarded(callSpan, 1, evt)
		case agent.EventTypeCitation:
			citations = append(citations, citationSources(evt.Payload)...)
			emitter.emitForwarded(callSpan, 1, evt)
		case agent.EventTypeStatus:
			// The sub-agent's terminal status is not forwarded; capture a
			// failure reason so the observation is honest.
			if st := statusError(evt.Payload); st != "" {
				subFailed = st
			}
		default:
			// reasoning, plan, tool_call, tool_result, agent_call/result — all
			// forwarded nested under the delegation call for full transparency.
			emitter.emitForwarded(callSpan, 1, evt)
		}
	}

	res.LatencyMs = time.Since(start).Milliseconds()
	res.Content = strings.TrimSpace(answer.String())
	res.Success = subFailed == "" && res.Content != ""
	if subFailed != "" {
		res.Error = subFailed
	} else if res.Content == "" {
		res.Error = "sub-agent returned no answer"
	}

	summary := res.Content
	if len(summary) > cfg.MaxArtifactSummaryBytes {
		summary = summary[:cfg.MaxArtifactSummaryBytes] + "..."
	}
	if !res.Success && summary == "" {
		summary = res.Error
	}
	emitter.emitAgentResult(callSpan, rootSpan, actor, &agent.AgentResultPayload{
		AgentID: d.AgentName, Success: res.Success, Summary: summary, LatencyMs: res.LatencyMs, Error: res.Error,
	})
	return res, citations
}

// delegationQuery extracts the sub-question for a delegation from the planner's
// chosen arguments (question/query/task), falling back to the original query.
func delegationQuery(step ToolPlanStep, fallback string) string {
	for _, k := range []string{"question", "query", "task", "input"} {
		if v, ok := step.Arguments[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return fallback
}

// decodeDelegatedEvent decodes a sub-agent SSE data payload into a StreamEvent.
// It accepts both a bare StreamEvent and an A2A JSON-RPC envelope wrapping one
// under "result". Returns nil when neither shape yields a typed event.
func decodeDelegatedEvent(raw json.RawMessage) *agent.StreamEvent {
	var evt agent.StreamEvent
	if err := json.Unmarshal(raw, &evt); err == nil && evt.Type != "" {
		return &evt
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Result) > 0 {
		var inner agent.StreamEvent
		if err := json.Unmarshal(env.Result, &inner); err == nil && inner.Type != "" {
			return &inner
		}
	}
	return nil
}

// artifactText extracts the concatenated text from an artifact payload. The
// payload arrives as map[string]any (a StreamEvent unmarshalled with Payload any).
func artifactText(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	parts, ok := m["parts"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if t, ok := pm["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// citationSources extracts citation sources from a citation payload map.
func citationSources(payload any) []agent.CitationSource {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	srcs, ok := m["sources"].([]any)
	if !ok {
		return nil
	}
	out := make([]agent.CitationSource, 0, len(srcs))
	for _, s := range srcs {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		cs := agent.CitationSource{}
		if v, ok := sm["type"].(string); ok {
			cs.Type = v
		}
		if v, ok := sm["id"].(string); ok {
			cs.ID = v
		}
		if v, ok := sm["label"].(string); ok {
			cs.Label = v
		}
		if cs.ID != "" || cs.Label != "" {
			out = append(out, cs)
		}
	}
	return out
}

// statusError returns the failure message from a status payload map when the
// state is "failed", else "".
func statusError(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	if state, _ := m["state"].(string); state != agent.TaskStateFailed {
		return ""
	}
	if em, ok := m["error"].(map[string]any); ok {
		if msg, ok := em["message"].(string); ok && msg != "" {
			return msg
		}
	}
	return "sub-agent failed"
}

// --- eventEmitter: helper for sequence numbering + event construction ---

type eventEmitter struct {
	taskID string
	actor  agent.EventActor
	seq    atomic.Int64
	out    chan<- *agent.StreamEvent
}

func newEmitter(out chan<- *agent.StreamEvent, taskID string, actor agent.EventActor) *eventEmitter {
	return &eventEmitter{taskID: taskID, actor: actor, out: out}
}

func (e *eventEmitter) emit(evt *agent.StreamEvent) {
	evt.TaskID = e.taskID
	evt.Sequence = int(e.seq.Add(1))
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	if evt.Actor.Type == "" {
		evt.Actor = e.actor
	}
	e.out <- evt
}

func (e *eventEmitter) emitReasoning(spanID, parentSpan string, depth int, text string, append bool) {
	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Depth:        depth,
		Type:         agent.EventTypeReasoning,
		Payload:      &agent.ReasoningPayload{Text: text, Append: append},
	})
}

func (e *eventEmitter) emitPlan(spanID string, plan *ToolPlan) {
	steps := make([]agent.PlanStep, 0, len(plan.Steps))
	for i, s := range plan.Steps {
		desc := s.Rationale
		if desc == "" {
			desc = "Call " + s.ToolName
			if s.Name != "" {
				desc = s.Name + ": " + desc
			}
		}
		steps = append(steps, agent.PlanStep{
			Index:       i,
			Description: desc,
			Tool:        s.ToolName,
			DependsOn:   s.DependsOn,
		})
	}
	e.emit(&agent.StreamEvent{
		SpanID:  spanID,
		Type:    agent.EventTypePlan,
		Payload: &agent.PlanPayload{Steps: steps},
	})
}

func (e *eventEmitter) emitToolCall(spanID, parentSpan string, step ToolPlanStep, idx int) {
	toolActor := agent.EventActor{
		Type:        "tool",
		ID:          "mcp:" + step.ToolName,
		DisplayName: step.ToolName,
	}
	// Strip _prior_result from the operator-facing chip args. Today the
	// executor clones step.Arguments before injecting _prior_result so this
	// is a no-op pre-resolution, but stripping defensively means the chip
	// stays clean even if a future change reorders emit/resolve.
	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Depth:        2,
		Actor:        toolActor,
		Type:         agent.EventTypeToolCall,
		Payload: &agent.ToolCallPayload{
			ToolName:  step.ToolName,
			Arguments: stripPriorResult(step.Arguments),
			StepIndex: idx,
		},
	})
}

func (e *eventEmitter) emitToolCallSkipped(spanID, parentSpan string, step ToolPlanStep, idx int, reason string) {
	toolActor := agent.EventActor{
		Type:        "tool",
		ID:          "mcp:" + step.ToolName,
		DisplayName: step.ToolName,
	}
	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Depth:        2,
		Actor:        toolActor,
		Type:         agent.EventTypeToolCall,
		Payload: &agent.ToolCallPayload{
			ToolName:   step.ToolName,
			Arguments:  stripPriorResult(step.Arguments),
			StepIndex:  idx,
			Skipped:    true,
			SkipReason: reason,
		},
	})
}

func (e *eventEmitter) emitToolResult(spanID, parentSpan string, result *ToolStepResult, cfg Config) {
	toolActor := agent.EventActor{
		Type:        "tool",
		ID:          "mcp:" + result.ToolName,
		DisplayName: result.ToolName,
	}

	preview := result.Content
	truncated := false
	if len(preview) > cfg.MaxResultPreviewBytes {
		preview = preview[:cfg.MaxResultPreviewBytes]
		truncated = true
	}

	summary := result.Content
	if len(summary) > cfg.MaxArtifactSummaryBytes {
		summary = summary[:cfg.MaxArtifactSummaryBytes] + "..."
	}
	if !result.Success {
		summary = result.Error
	}

	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Depth:        2,
		Actor:        toolActor,
		Type:         agent.EventTypeToolResult,
		Payload: &agent.ToolResultPayload{
			StepIndex:       result.StepIndex,
			ToolName:        result.ToolName,
			Success:         result.Success,
			Summary:         summary,
			ResultPreview:   preview,
			ResultSizeBytes: len(result.Content),
			Truncated:       truncated,
			LatencyMs:       result.LatencyMs,
			ErrorCode:       result.ErrorCode,
		},
	})
}

func (e *eventEmitter) emitCitation(spanID string, sources []agent.CitationSource) {
	e.emit(&agent.StreamEvent{
		SpanID:  spanID,
		Type:    agent.EventTypeCitation,
		Payload: &agent.CitationPayload{Sources: sources},
	})
}

// emitUsage emits a task/usage event carrying token usage for one ReAct
// decision turn, the terminal assembly call, or the per-request aggregate
// (OGA-420). available=false labels the counts as not reported by the proxy.
func (e *eventEmitter) emitUsage(spanID, parentSpan, role string, turnIndex int, model string, usage agent.TokenUsage, available bool) {
	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Type:         agent.EventTypeUsage,
		Payload: &agent.UsagePayload{
			Role:      role,
			TurnIndex: turnIndex,
			Model:     model,
			Usage:     usage,
			Available: available,
		},
	})
}

func (e *eventEmitter) emitArtifact(spanID string, text string, append bool) {
	e.emit(&agent.StreamEvent{
		SpanID: spanID,
		Type:   agent.EventTypeArtifact,
		Payload: &agent.ArtifactPayload{
			Parts:  []agent.ArtifactPart{{Text: text}},
			Append: append,
		},
	})
}

func (e *eventEmitter) emitStatus(spanID string, state string, statusErr *agent.StatusError) {
	e.emit(&agent.StreamEvent{
		SpanID:  spanID,
		Type:    agent.EventTypeStatus,
		Payload: &agent.StatusPayload{State: state, Error: statusErr},
	})
}

func (e *eventEmitter) emitAgentCall(spanID, parentSpan string, actor agent.EventActor, payload *agent.AgentCallPayload) {
	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Depth:        1,
		Actor:        actor,
		Type:         agent.EventTypeAgentCall,
		Payload:      payload,
	})
}

func (e *eventEmitter) emitAgentResult(spanID, parentSpan string, actor agent.EventActor, payload *agent.AgentResultPayload) {
	e.emit(&agent.StreamEvent{
		SpanID:       spanID,
		ParentSpanID: parentSpan,
		Depth:        1,
		Actor:        actor,
		Type:         agent.EventTypeAgentResult,
		Payload:      payload,
	})
}

// emitForwarded re-emits a sub-agent's StreamEvent nested under the delegation
// call span. The sub-agent's Actor is preserved (so the UI attributes the work
// to the sub-agent); its span is re-parented to the call span when it is a
// root-level sub-agent event, and its depth is offset so it renders nested.
// emit() re-stamps TaskID + Sequence with this task's numbering.
func (e *eventEmitter) emitForwarded(callSpan string, callDepth int, evt *agent.StreamEvent) {
	fwd := *evt
	if fwd.ParentSpanID == "" {
		fwd.ParentSpanID = callSpan
	}
	fwd.Depth = callDepth + 1 + evt.Depth
	// Clear the wire-stamped numbering so emit() assigns this task's own.
	fwd.TaskID = ""
	fwd.Sequence = 0
	e.emit(&fwd)
}

// Compile-time check: the gateway package's PlatformGatewayClient must
// satisfy the streampipeline PlatformAccess interface so callers can pass
// it directly without an adapter.
var _ PlatformAccess = (*gateway.PlatformGatewayClient)(nil)

// Suppress unused imports during incremental development — json + uuid are
// re-used by the planner constructors.
var _ = json.Marshal
var _ = uuid.New
