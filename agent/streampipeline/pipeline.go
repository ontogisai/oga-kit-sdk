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

	// ReAct loop: decide ONE action per turn against the full observation
	// transcript, execute it, observe, repeat — until the planner finalizes or
	// the step budget / no-progress guard stops it (OGA-419).
	plannerQuery := input.Query
	if strings.TrimSpace(input.PlannerQuery) != "" {
		plannerQuery = input.PlannerQuery
	}

	results := make([]ToolStepResult, 0, cfg.MaxSteps)
	allCitations := make([]agent.CitationSource, 0)
	decided := make([]ToolPlanStep, 0, cfg.MaxSteps) // for the evolving task/plan

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
				return fmt.Errorf("planner.Next: %w", err)
			}
			// No observations yet → degrade to a plain LLM answer (mirrors the
			// pre-OGA-419 fallback). If the gateway is genuinely down, the
			// assembly call inside runPlainAnswer fails and surfaces
			// task/status{failed}, so a real transport failure is never masked.
			if len(results) == 0 {
				logger.WarnContext(ctx, "streampipeline: first-turn planning failed, falling back to plain answer",
					"error", err)
				return p.runPlainAnswer(ctx, gw, input, cfg, tracker, rootSpan, emitter,
					"Tool planning was unavailable; answering directly from the model.")
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
			return ctx.Err()
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

		result := executeStep(ctx, gw, step, idx, results, cfg.ToolTimeout)
		emitter.emitToolResult(toolSpan, rootSpan, &result, cfg)

		// Required-step failure → fail-fast (honored for grounding/seed steps).
		if step.Required && !result.Success && !result.Skipped {
			emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
				Code:    -32000,
				Message: fmt.Sprintf("required step %q failed: %s", step.Name, result.Error),
			})
			return fmt.Errorf("required step %q failed: %s", step.Name, result.Error)
		}

		results = append(results, result)

		// Extract + emit citations.
		citations := ExtractCitations(&result, step.ToolName, step.Arguments)
		if len(citations) > 0 {
			emitter.emitCitation(toolSpan, citations)
			allCitations = append(allCitations, citations...)
		}
	}

	// 3. Assembly
	emitter.emitReasoning(tracker.ChildSpan(rootSpan), rootSpan, 1, "Assembling response...", false)

	if err := p.streamAssembly(ctx, gw, input, cfg, tracker, rootSpan, emitter, results); err != nil {
		emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
			Code:    -32000,
			Message: "assembly failed: " + err.Error(),
		})
		return fmt.Errorf("assembly: %w", err)
	}

	// 4. Consolidated citation
	if len(allCitations) > 0 {
		emitter.emitCitation(rootSpan, allCitations)
	}

	// 5. Final status
	emitter.emitStatus(rootSpan, agent.TaskStateCompleted, nil)
	return nil
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

	if err := p.streamAssembly(ctx, gw, input, cfg, tracker, rootSpan, emitter, nil); err != nil {
		emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
			Code:    -32000,
			Message: "assembly failed: " + err.Error(),
		})
		return err
	}
	emitter.emitStatus(rootSpan, agent.TaskStateCompleted, nil)
	return nil
}

// streamAssembly builds the assembly prompt from prior tool results and
// streams the LLM response as task/artifact events. Falls back to a single
// chat completion when streaming is unavailable.
func (p *Pipeline) streamAssembly(
	ctx context.Context,
	gw PlatformAccess,
	input Input,
	cfg Config,
	tracker *agent.SpanTracker,
	rootSpan string,
	emitter *eventEmitter,
	results []ToolStepResult,
) error {
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

	req := &gateway.ChatCompletionRequest{
		Messages: []gateway.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens: 2048,
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

	// Try streaming first. Fall back to non-streaming on error.
	if gw != nil {
		req.Stream = true
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
				return nil
			}
			// Stream produced nothing — fall through to sync.
		}
	}

	// Non-streaming fallback.
	req.Stream = false
	resp, err := gw.ChatCompletion(asmCtx, req)
	if err != nil {
		return err
	}
	if len(resp.Choices) == 0 {
		return errors.New("no choices in assembly response")
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
	return nil
}

// runArtifact drives the pipeline but drains all events to a buffer and
// returns the assembled artifact text + consolidated citations. It backs the
// typed RunSync[T] (see runsync.go) and any non-streaming message/send path.
func (p *Pipeline) runArtifact(
	ctx context.Context,
	deps Deps,
	input Input,
	planner Planner,
) (string, []agent.CitationSource, error) {
	events := make(chan *agent.StreamEvent, 64)

	type result struct {
		artifact  strings.Builder
		citations []agent.CitationSource
	}
	final := &result{}
	done := make(chan error, 1)

	go func() {
		err := p.Run(ctx, deps, input, planner, events)
		close(events)
		done <- err
	}()

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
		}
	}

	err := <-done
	if traceEnabled() {
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.InfoContext(ctx, "trace: artifact assembled (stream-collected)",
			"actor", input.Actor.ID,
			"tenant_id", input.TenantID,
			"artifact_bytes", final.artifact.Len(),
			"citations", len(final.citations),
		)
	}
	return final.artifact.String(), final.citations, err
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

// Compile-time check: the gateway package's PlatformGatewayClient must
// satisfy the streampipeline PlatformAccess interface so callers can pass
// it directly without an adapter.
var _ PlatformAccess = (*gateway.PlatformGatewayClient)(nil)

// Suppress unused imports during incremental development — json + uuid are
// re-used by the planner constructors.
var _ = json.Marshal
var _ = uuid.New
