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
	}
}

// Deps wires platform services into the pipeline. The Gateway is the single
// access point — all MCP tool calls and LLM completions go through it for
// uniform PBAC, audit, rate limiting, and tenant attribution (OGA-303).
type Deps struct {
	Gateway *gateway.PlatformGatewayClient
	Logger  *slog.Logger
	Config  Config
}

// Input is the per-request configuration the pipeline needs from its caller.
type Input struct {
	// Query is the user's message text.
	Query string

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

	// ProactivePlaceholders resolves grounding-step argument placeholders for
	// the proactive path ({entity_id}, {entity_type}, {entity_properties.X},
	// {time_minus_24h}, ...). It is set by runProactiveReasoning from the
	// triggering ProactiveEvent. Nil on the reactive path — the LLMToolPlanner
	// emits concrete arguments, so there is nothing to substitute. See OGA-350.
	//
	// Distinct from the dependent-step (<from step N>) resolution the executor
	// applies via agent.ResolveDependentArgsForTool (OGA-331) — see the
	// two-conventions note in placeholders.go.
	ProactivePlaceholders ProactivePlaceholderResolver
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
	planner StreamPlanner,
	events chan<- *agent.StreamEvent,
) error {
	return p.runInternal(ctx, deps, input, planner, events, deps.Gateway)
}

// runInternal is the test seam: it accepts the gatewayClient interface
// directly so tests can inject a fake without constructing a real
// *gateway.PlatformGatewayClient. Production callers go through Run.
func (p *Pipeline) runInternal(
	ctx context.Context,
	deps Deps,
	input Input,
	planner StreamPlanner,
	events chan<- *agent.StreamEvent,
	gw gatewayClient,
) error {
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
	emitter := newEmitter(events, uuid.New().String(), input.Actor)

	// 1. Plan
	plan, narrative, err := planner.Plan(ctx, input.Query, input.ToolNames)
	if err != nil {
		emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
			Code:    -32000,
			Message: "planning failed: " + err.Error(),
		})
		return fmt.Errorf("planner.Plan: %w", err)
	}

	if narrative != nil && narrative.Text != "" {
		emitter.emitReasoning(tracker.ChildSpan(rootSpan), rootSpan, 1, narrative.Text, false)
	}

	if plan == nil || len(plan.Steps) == 0 {
		// No plan — fall through to plain LLM answer (no tool grounding).
		return p.runPlainAnswer(ctx, gw, input, cfg, tracker, rootSpan, emitter)
	}

	// Resolve proactive event placeholders ({entity_id}, {entity_properties.X},
	// {time_minus_24h}, ...) into the plan's step arguments before they are
	// emitted or executed. No-op on the reactive path (ProactivePlaceholders is
	// nil). Done after the empty-plan check so the LLM path skips it entirely,
	// and before emitPlan so chips show resolved values. See OGA-350.
	substitutePlan(ctx, plan, input.ProactivePlaceholders, logger)

	if len(plan.Steps) > cfg.MaxSteps {
		logger.WarnContext(ctx, "streampipeline: plan exceeds MaxSteps, truncating",
			"plan_steps", len(plan.Steps),
			"max_steps", cfg.MaxSteps,
		)
		plan.Steps = plan.Steps[:cfg.MaxSteps]
	}

	emitter.emitPlan(rootSpan, plan)

	// 2. Execute steps
	results := make([]ToolStepResult, 0, len(plan.Steps))
	allCitations := make([]agent.CitationSource, 0)

	for i, step := range plan.Steps {
		if ctx.Err() != nil {
			emitter.emitStatus(rootSpan, agent.TaskStateCanceled, &agent.StatusError{
				Code:    -32000,
				Message: "cancelled: " + ctx.Err().Error(),
			})
			return ctx.Err()
		}

		toolSpan := tracker.ChildSpan(rootSpan)

		// Evaluate Condition.
		shouldRun, skipReason := evaluateCondition(step.Condition)
		if !shouldRun {
			// Emit a skipped tool_call (no tool_result, no citation).
			emitter.emitToolCallSkipped(toolSpan, rootSpan, step, i, skipReason)
			results = append(results, ToolStepResult{
				StepIndex:  i,
				ToolName:   step.ToolName,
				Skipped:    true,
				SkipReason: skipReason,
			})
			continue
		}

		// Emit tool_call.
		emitter.emitToolCall(toolSpan, rootSpan, step, i)

		// Execute.
		result := executeStep(ctx, gw, step, i, results, cfg.ToolTimeout)
		results = append(results, result)

		// Emit tool_result.
		emitter.emitToolResult(toolSpan, rootSpan, &result, cfg)

		// Required step failure → fail-fast.
		if step.Required && !result.Success && !result.Skipped {
			emitter.emitStatus(rootSpan, agent.TaskStateFailed, &agent.StatusError{
				Code:    -32000,
				Message: fmt.Sprintf("required step %q failed: %s", step.Name, result.Error),
			})
			return fmt.Errorf("required step %q failed: %s", step.Name, result.Error)
		}

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
// greeting, or LLM judges no tools needed). Streams a single LLM response as
// task/artifact, no plan / tool / citation events.
func (p *Pipeline) runPlainAnswer(
	ctx context.Context,
	gw gatewayClient,
	input Input,
	cfg Config,
	tracker *agent.SpanTracker,
	rootSpan string,
	emitter *eventEmitter,
) error {
	emitter.emitReasoning(tracker.ChildSpan(rootSpan), rootSpan, 1, "No tool calls needed; answering directly.", false)

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
	gw gatewayClient,
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
				}
			}
			if anyContent {
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
	planner StreamPlanner,
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
// satisfy the streampipeline gatewayClient interface so callers can pass
// it directly without an adapter.
var _ gatewayClient = (*gateway.PlatformGatewayClient)(nil)

// Suppress unused imports during incremental development — json + uuid are
// re-used by the planner constructors.
var _ = json.Marshal
var _ = uuid.New
