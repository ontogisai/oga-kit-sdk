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
	args := step.Arguments
	if args == nil {
		args = make(map[string]any)
	}
	if step.DependsOn >= 0 && step.DependsOn < len(priorResults) {
		prior := priorResults[step.DependsOn]
		if prior.Success && prior.Content != "" {
			args = agent.ResolveDependentArgs(args, prior.Content)
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
		res.Content = truncateJSONArray(res.Content, step.MaxResults)
	}

	return res
}

// truncateJSONArray caps a JSON `{"results": [...]}` shape at maxResults
// entries. Returns the original content if not parseable as the expected shape
// (truncation is a best-effort optimization, not a correctness requirement).
func truncateJSONArray(content string, maxResults int) string {
	if maxResults <= 0 {
		return content
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return content
	}
	results, ok := obj["results"].([]any)
	if !ok || len(results) <= maxResults {
		return content
	}
	obj["results"] = results[:maxResults]
	truncated, err := json.Marshal(obj)
	if err != nil {
		return content
	}
	return string(truncated)
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
