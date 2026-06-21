// Package streampipeline is the shared streaming orchestrator used by the
// platform's Knowledge Agent and every kit-supplied domain agent (OGA-303,
// OGA-419).
//
// The pipeline drives an interleaved ReAct loop (Reason -> Act -> Observe ->
// repeat): one decision per turn, made against the full transcript of prior
// observations. This replaces the previous plan-once-then-execute model
// (OGA-419). The canonical event sequence per turn:
//
//	task/reasoning (the "Thought")
//	  -> task/plan (evolving — the decided step appended)
//	  -> task/tool_call (the "Action")
//	  -> task/tool_result (the "Observation")
//	  -> task/citation
//	... repeat until the planner signals Done ...
//	  -> task/reasoning ("Assembling response...")
//	  -> token-streamed task/artifact
//	  -> consolidated task/citation
//	  -> task/status{completed}
//
// Planners:
//   - LLMToolPlanner — calls the LLM via PlatformAccess each turn to pick the
//     next tool dynamically against the observations so far (reactive chat,
//     Knowledge Agent, and the proactive proposal path seeded with grounding
//     hints).
//   - InvestigationLLMPlanner — front-loads a deterministic kg_get_entity per
//     concrete seed entity (guaranteed grounding), then delegates to an inner
//     LLMToolPlanner for question-relevant evidence.
package streampipeline

import (
	"context"
	"encoding/json"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// Planner decides the next action given the observations so far — ONE call per
// turn. The pipeline emits the narrative as a task/reasoning event, executes
// the returned step, appends the observation to PlanState.History, and calls
// Next again until Done is true.
//
// Implementations are expected to be stateless across turns: all the state a
// planner needs (query, persona, grounding hints, seed facts, observation
// history, remaining budget) is supplied in PlanState each turn. This keeps a
// single Planner instance safe to reuse and the loop fully transcript-driven.
type Planner interface {
	// Next returns the next action to take. When Done is true, Step is nil and
	// the pipeline proceeds to the terminal step (assembly or RunSync decision).
	Next(ctx context.Context, st *PlanState) (*Decision, error)
}

// PlanState is the full context handed to the planner on every turn. The
// pipeline owns it and appends to History after each executed step.
type PlanState struct {
	// Query is the user/seed query handed to the planner. On the investigation
	// path it carries the PLANNING-framed proposal context (OGA-398).
	Query string

	// Persona is the system prompt + tool palette for this agent/path. The
	// Tools slice bounds what the planner may call (the guardrail).
	Persona PlannerPersona

	// GroundingStrategy is the kit-declared grounding strategy surfaced as
	// ADVISORY hints (recommended tools, arguments, ordering, rationale). It is
	// populated whenever the agent has a profile strategy (domain agents, on
	// BOTH the proactive and reactive paths — OGA-419), and empty for
	// profile-less platform agents (the Knowledge Agent). Under the ReAct loop
	// these are hints, never a forced chain: Condition is a suggested-when hint,
	// Required is strongly-advised, DependsOn is an ordering hint, MaxResults is
	// an honored cap.
	GroundingStrategy []agent.GroundingStep

	// SeedFacts is resolved factual context the planner should ground on without
	// re-deriving it. Proactive: the resolved triggering-event facts. Reactive
	// domain Investigate: the investigation context (which proposal is under
	// investigation). Empty for plain reactive chat (the Knowledge Agent).
	SeedFacts string

	// History is every executed step's result so far, in order. The planner
	// reasons over this to decide the next action (or to stop).
	History []ToolStepResult

	// StepBudget is the number of turns remaining before the pipeline's
	// max_steps cap forces termination.
	StepBudget int
}

// PlannerPersona decouples the planner from agent.DomainAgentProfile so the
// Knowledge Agent (which has no profile) can drive the same loop. A domain
// agent builds it from its profile; the KA builds it from its planner prompt +
// kg_* tool union.
type PlannerPersona struct {
	// SystemPrompt is the planner's persona/instructions for the decision call.
	SystemPrompt string

	// Tools is the union of MCP tool names available to this agent on this path
	// — the palette that bounds what the planner may call.
	Tools []string

	// ToolSchemas optionally carries richer per-tool detail (description +
	// JSON-Schema inputs) for the tools in the palette. When present, the
	// decision prompt renders each tool's argument names + types so the model
	// emits correct arguments instead of guessing (OGA-419 G1). The Knowledge
	// Agent populates these from discovered MCP tools; tools without a matching
	// schema render as bare names.
	ToolSchemas []agent.ToolSchema
}

// Decision is the planner's output for a single turn.
type Decision struct {
	// Done signals the loop to stop and run the terminal step. When true, Step
	// is nil.
	Done bool

	// Narrative is the human-readable "Thought" emitted as a task/reasoning
	// event before the action. May be empty.
	Narrative string

	// Step is the action to execute this turn. Nil iff Done.
	Step *ToolPlanStep

	// Usage is the LLM token usage the planner spent producing this decision
	// (OGA-420). Zero for non-LLM planners (e.g. precomputed seed steps).
	Usage agent.TokenUsage

	// UsageAvailable is true when Usage carries real counts from the proxy.
	UsageAvailable bool
}

// PlanNarrative is the human-readable text emitted as a task/reasoning event.
// Retained for planner constructors that surface a leading narrative.
type PlanNarrative struct {
	Text string
}

// ToolPlan is an ordered set of steps. It is retained as the shape a planner
// may precompute internally (e.g. the investigation planner's seed steps) and
// as the payload for the evolving task/plan event. The pipeline no longer
// executes a ToolPlan wholesale — it drives Planner.Next turn by turn.
type ToolPlan struct {
	Steps []ToolPlanStep
}

// ToolPlanStep is one step in an execution plan / a single Decision action.
//
// Fields Required, Condition, MaxResults preserve the kit-author-facing
// semantics from the GroundingStrategy YAML schema. The LLM decision call
// leaves them at zero/empty defaults.
type ToolPlanStep struct {
	// Name is the human-readable identifier and placeholder key.
	Name string

	// ToolName is the MCP tool to invoke (e.g., "kg_search").
	ToolName string

	// Arguments is the parameter map passed to the tool.
	Arguments map[string]any

	// DependsOn is the index of the prior step this step needs (for placeholder
	// resolution), or -1 for no dependency. Under ReAct the LLM usually emits
	// concrete arguments (it has observed the prior result), so this is mostly
	// vestigial on the LLM path and load-bearing only for precomputed seeds.
	DependsOn int

	// Rationale is a short human-readable explanation included in task/plan.
	Rationale string

	// Required marks fail-fast on the proactive/grounding path: a tool error
	// stops the pipeline. On the ReAct LLM path it is advisory.
	Required bool

	// Condition is a CEL expression (advisory under ReAct). Empty / "true" =
	// run; "false" = skip; other expressions are not yet evaluated.
	Condition string

	// MaxResults caps the size of a JSON-array result. 0 = no cap.
	MaxResults int
}

// ToolStepResult captures the outcome of executing one ToolPlanStep — i.e. one
// Observation in the ReAct transcript.
type ToolStepResult struct {
	StepIndex int
	ToolName  string
	Success   bool
	Content   string
	Result    json.RawMessage
	Error     string
	ErrorCode string
	LatencyMs int64

	// Skipped is true when the step was skipped before execution (Condition
	// false, or upstream dependency had empty results).
	Skipped    bool
	SkipReason string
}
