package streampipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// gatewayClient is the minimal subset of *gateway.PlatformGatewayClient the
// pipeline uses. Defined as an interface so tests can supply a fake.
type gatewayClient interface {
	CallTool(ctx context.Context, tool string, params any) (json.RawMessage, error)
	ChatCompletion(ctx context.Context, req *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error)
	ChatCompletionStream(ctx context.Context, req *gateway.ChatCompletionRequest) (<-chan *gateway.ChatChunk, error)
}

// executeStep runs one ToolPlanStep against the gateway, applying placeholder
// substitution from prior step results, MaxResults truncation, and timeout.
// It does NOT evaluate Condition — the pipeline does that before calling here.
func executeStep(
	ctx context.Context,
	gw gatewayClient,
	step ToolPlanStep,
	stepIndex int,
	priorResults []ToolStepResult,
	timeout time.Duration,
) ToolStepResult {
	res := ToolStepResult{StepIndex: stepIndex, ToolName: step.ToolName}

	// Resolve placeholder arguments from prior step results.
	//
	// Clone step.Arguments before resolution so ResolveDependentArgs (which
	// mutates the map by injecting "_prior_result" and resolving placeholder
	// IDs in place) doesn't leak through to either:
	//   1. The task/tool_call event payload — it holds a reference to
	//      step.Arguments for the chip display, and JSON marshaling at the
	//      consumer side happens after this function runs. Without a clone,
	//      the chip ends up showing post-mutation state.
	//   2. Subsequent re-runs of the same plan — the mutation persists in
	//      the shared map across runs.
	args := cloneArgs(step.Arguments)
	if step.DependsOn >= 0 && step.DependsOn < len(priorResults) {
		prior := priorResults[step.DependsOn]
		if prior.Success && prior.Content != "" {
			// Dependent-step (<from step N>) resolution — fills ID args from a
			// prior tool result (OGA-331). This is the SECOND of the two
			// placeholder conventions; the FIRST ({entity_id} event templates)
			// is already resolved at plan-build time by substitutePlan. See the
			// two-conventions note in placeholders.go. Concrete values from
			// event substitution are never overwritten here (needsResolution).
			args = agent.ResolveDependentArgsForTool(args, prior.Content, prior.ToolName)
		} else if !prior.Success || prior.Content == "" {
			// Short-circuit: upstream returned nothing usable.
			res.Success = false
			res.Skipped = true
			res.SkipReason = fmt.Sprintf("upstream step %d returned no results", step.DependsOn)
			return res
		}
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	raw, err := gw.CallTool(stepCtx, step.ToolName, args)
	res.LatencyMs = time.Since(start).Milliseconds()

	if err != nil {
		res.Success = false
		// Preserve structured tool error if present.
		if toolErr, ok := err.(*gateway.ToolError); ok {
			res.Error = toolErr.Error()
			res.ErrorCode = toolErr.Code
		} else {
			res.Error = err.Error()
		}
		return res
	}

	res.Success = true
	res.Result = raw
	res.Content = string(raw)

	// Apply MaxResults truncation if configured.
	if step.MaxResults > 0 {
		res.Content = truncateJSONArray(res.Content, step.MaxResults, step.ToolName)
	}

	return res
}

// cloneArgs returns a shallow copy of args. Used by executeStep to avoid
// mutating the original step.Arguments — important because the tool_call
// event emitted before execute holds a reference to that same map and
// JSON-marshals it later at the consumer side. Without the clone, the chip
// would render post-mutation state (including the bulky _prior_result
// injection) rather than the LLM-planned arguments.
//
// Shallow because nested values are not mutated by ResolveDependentArgs —
// only top-level scalar keys (entity_id, start_entity_id, …) get rewritten,
// and a new top-level _prior_result key is added.
func cloneArgs(args map[string]any) map[string]any {
	if args == nil {
		return make(map[string]any)
	}
	out := make(map[string]any, len(args)+1) // +1 for _prior_result if added
	for k, v := range args {
		out[k] = v
	}
	return out
}

// stripPriorResult returns a shallow copy of args with the _prior_result key
// removed. Used at chip emission sites so the operator UI displays only the
// LLM-planned arguments (mode, start_entity_id, stop_conditions, …) and not
// the bulky internal injection that ResolveDependentArgs adds for handlers.
//
// The injection still flows to the gateway via executeStep — handlers that
// re-parse it for fallback ID resolution keep working.
func stripPriorResult(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	if _, has := args["_prior_result"]; !has {
		return args
	}
	out := make(map[string]any, len(args)-1)
	for k, v := range args {
		if k == "_prior_result" {
			continue
		}
		out[k] = v
	}
	return out
}

// truncateJSONArray caps the tool's result array at maxResults entries. The
// array key is resolved from the shape registry for toolName (e.g. kg_search →
// "results", kg_query_entities → "entities", kg_doc_search → "documents"); for
// unknown tools it falls back to a default set of common array keys. Returns
// the original content if not parseable as the expected shape (truncation is a
// best-effort optimization, not a correctness requirement).
func truncateJSONArray(content string, maxResults int, toolName string) string {
	if maxResults <= 0 {
		return content
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return content
	}

	keys := agent.ArrayKeysForTool(toolName)
	if len(keys) == 0 {
		keys = []string{"results", "entities", "nodes", "documents", "passages", "relationships"}
	}

	for _, key := range keys {
		arr, ok := obj[key].([]any)
		if !ok || len(arr) <= maxResults {
			continue
		}
		obj[key] = arr[:maxResults]
		truncated, err := json.Marshal(obj)
		if err != nil {
			return content
		}
		return string(truncated)
	}
	return content
}

// evaluateCondition checks whether a step should run.
//
// For OGA-303 PR1, this supports literal evaluations only:
//   - empty / "true" → run
//   - "false" → skip (with reason "condition evaluated false")
//   - anything else → skip with reason "CEL evaluation not yet implemented"
//
// Full CEL integration (using google/cel-go on the platform side, or a
// lightweight evaluator on the kit side) is tracked as a follow-up. Until
// then, kit authors who declare CEL expressions get explicit skip events
// rather than silent failures.
func evaluateCondition(condition string) (run bool, reason string) {
	c := strings.TrimSpace(condition)
	if c == "" || c == "true" {
		return true, ""
	}
	if c == "false" {
		return false, "condition evaluated false"
	}
	return false, fmt.Sprintf("CEL evaluation not yet implemented; treating as skip (expression: %q)", condition)
}
