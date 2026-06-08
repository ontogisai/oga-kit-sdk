// Package streampipeline is the shared streaming orchestrator used by the
// platform's Knowledge Agent and every kit-supplied domain agent (OGA-303).
//
// Both consumers feed a StreamPlanner implementation to Pipeline.Run, which
// drives the canonical event sequence:
//
//	task/reasoning (planner narrative)
//	  → task/plan
//	  → per-step task/tool_call → task/tool_result → task/citation
//	  → task/reasoning ("Assembling response...")
//	  → token-streamed task/artifact
//	  → consolidated task/citation
//	  → task/status{completed}
//
// The Knowledge Agent supplies an LLMToolPlanner (dynamic per-request planning).
// Domain agents supply a GroundingStrategyPlanner (deterministic from kit YAML)
// when their profile declares one, otherwise fall back to LLMToolPlanner.
package streampipeline

import (
	"context"
	"encoding/json"
)

// StreamPlanner produces the ordered execution plan for a single user query.
// The pipeline emits the narrative as a task/reasoning event before executing
// the steps.
//
// Implementations:
//   - LLMToolPlanner — calls the LLM via the Platform Access Gateway to pick
//     tools dynamically.
//   - GroundingStrategyPlanner — converts a kit-declared grounding strategy
//     YAML deterministically (no LLM call).
type StreamPlanner interface {
	// Plan returns the execution plan + a short narrative describing what the
	// planner is about to do (e.g., "Planning which tools to use..." or
	// "Investigating with FM Operations persona...").
	//
	// The tools argument is the union of MCP tool names available to this
	// agent (typically derived from profile.Capabilities[*].Tools). Planners
	// that don't need it (e.g., GroundingStrategyPlanner) ignore it.
	Plan(ctx context.Context, query string, tools []string) (*ToolPlan, *PlanNarrative, error)
}

// PlanNarrative is the human-readable text emitted as a task/reasoning event
// immediately before the task/plan event.
type PlanNarrative struct {
	Text string
}

// ToolPlan is the execution plan a StreamPlanner returns.
type ToolPlan struct {
	Steps []ToolPlanStep
}

// ToolPlanStep is one step in an execution plan.
//
// Fields Required, Condition, MaxResults preserve the kit-author-facing
// semantics from the GroundingStrategy YAML schema. The LLMToolPlanner
// leaves them at zero/empty defaults.
type ToolPlanStep struct {
	// Name is the human-readable identifier and placeholder key. The
	// LLMToolPlanner generates step_0, step_1, ... when the LLM doesn't
	// supply names.
	Name string

	// ToolName is the MCP tool to invoke (e.g., "kg_search").
	ToolName string

	// Arguments is the parameter map passed to the tool.
	Arguments map[string]any

	// DependsOn is the index of the prior step this step needs (for
	// placeholder resolution), or -1 for no dependency.
	DependsOn int

	// Rationale is a short human-readable explanation included in the
	// task/plan event.
	Rationale string

	// Required marks fail-fast: a tool error stops the pipeline with
	// task/status{failed}. Non-required steps log + continue.
	Required bool

	// Condition is a CEL expression evaluated at runtime. Empty / "true" =
	// always run; "false" = always skip; other expressions are not yet
	// evaluated (treated as skip with a "CEL evaluation not yet implemented"
	// reason). Full CEL integration is a follow-up.
	Condition string

	// MaxResults caps the size of a JSON-array result. 0 = no cap.
	MaxResults int
}

// ToolStepResult captures the outcome of executing one ToolPlanStep.
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
