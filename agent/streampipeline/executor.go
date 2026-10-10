package streampipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// PlatformAccess is the contract the pipeline depends on for tool calls, LLM
// completions, and (reactive) agent delegation. Named for the ROLE it provides,
// NOT for one implementor: *gateway.PlatformGatewayClient (kit sidecars, via the
// Platform Gateway) and the platform Knowledge Agent's adapter (direct to the
// MCP tool server + LLM proxy) both satisfy it. Defined as an interface so tests
// can supply a fake.
//
// InvokeAgentStream backs the reactive `ask_knowledge_agent` delegation
// (OGA-419). The real gateway client implements it; an adapter that does not
// delegate (e.g. the KA's own) may return an unsupported error — it is never
// called unless the agent's palette includes a delegation capability.
type PlatformAccess interface {
	CallTool(ctx context.Context, tool string, params any) (json.RawMessage, error)
	ChatCompletion(ctx context.Context, req *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error)
	ChatCompletionStream(ctx context.Context, req *gateway.ChatCompletionRequest) (<-chan *gateway.ChatChunk, error)
	InvokeAgentStream(ctx context.Context, agentName string, msg any) (<-chan *json.RawMessage, error)
}

// executeStep runs one ToolPlanStep against the gateway, applying placeholder
// substitution from prior step results, MaxResults truncation, and timeout.
// It does NOT evaluate Condition — the pipeline does that before calling here.
func executeStep(
	ctx context.Context,
	gw PlatformAccess,
	step ToolPlanStep,
	stepIndex int,
	priorResults []ToolStepResult,
	timeout time.Duration,
	schemas map[string]agent.ToolSchema,
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

	// Schema-aware pre-dispatch check (OGA-438): when the discovered tool
	// schema is available, verify the resolved args carry the schema's
	// top-level required fields and use valid enum values BEFORE spending a
	// gateway→MCP round-trip on a call the server would reject (OGA-CORE-VAL-1001
	// / OGA-MCPG-VAL-1002). On a violation we return a directive failure that
	// the ReAct loop feeds back to the planner, which self-corrects next turn.
	// Runs AFTER dependent-arg resolution so a field filled from a prior step is
	// not falsely flagged. Conditional requirements (e.g. source_id XOR
	// source_filter) live in the handler, NOT the JSON Schema `required` array,
	// so they are intentionally not enforced here — no false positives.
	if msg := validateToolArgs(schemas[step.ToolName].InputSchema, args); msg != "" {
		res.Success = false
		res.Error = fmt.Sprintf("invalid arguments for %s: %s", step.ToolName, msg)
		res.ErrorCode = "OGA-SDK-VAL-0001"
		return res
	}

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

// validateToolArgs performs a lightweight, false-positive-safe pre-dispatch
// check of a tool call's resolved arguments against its JSON Schema (OGA-438).
// It enforces exactly two things, mirroring what the MCP tool server validates
// first:
//
//   - top-level required fields are present and non-empty, and
//   - any present field constrained by an enum uses an allowed value.
//
// It deliberately does NOT attempt full JSON Schema validation: conditional /
// oneOf requirements are not in the schema's `required` array (they live in the
// handler), so enforcing only `required` + enum cannot block a legitimately
// valid call. Returns "" when the args pass or the schema is absent/unparseable
// (fail-open — never block on a schema we can't read).
func validateToolArgs(rawSchema json.RawMessage, args map[string]any) string {
	if len(rawSchema) == 0 {
		return ""
	}
	var doc struct {
		Properties map[string]struct {
			Type string `json:"type"`
			Enum []any  `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(rawSchema, &doc); err != nil {
		return ""
	}

	var missing []string
	for _, field := range doc.Required {
		if isMissingArg(args[field]) {
			missing = append(missing, field)
		}
	}

	var badEnum []string
	for name, prop := range doc.Properties {
		if len(prop.Enum) == 0 {
			continue
		}
		v, ok := args[name]
		if !ok {
			continue
		}
		s, isStr := v.(string)
		if !isStr || s == "" {
			continue // non-string or empty — required-check handles empties
		}
		if !enumContains(prop.Enum, s) {
			badEnum = append(badEnum, fmt.Sprintf("%s must be one of [%s] (got %q)",
				name, joinEnumValues(prop.Enum), s))
		}
	}

	if len(missing) == 0 && len(badEnum) == 0 {
		return ""
	}
	var parts []string
	if len(missing) > 0 {
		sort.Strings(missing)
		parts = append(parts, "missing required field(s): "+strings.Join(missing, ", "))
	}
	parts = append(parts, badEnum...)
	return strings.Join(parts, "; ")
}

// isMissingArg reports whether an argument value counts as absent for a
// required-field check: the key was missing (nil), or it is an empty string.
// A false/0 value is NOT missing — only nil and "" mirror the server's
// "empty or missing" semantics, so a required bool/number is never wrongly
// flagged.
func isMissingArg(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// enumContains reports whether s equals any enum value (stringified with %v so
// numeric/bool enums compare too).
func enumContains(enum []any, s string) bool {
	for _, e := range enum {
		if fmt.Sprintf("%v", e) == s {
			return true
		}
	}
	return false
}

// joinEnumValues renders enum values as "a, b, c", bounded so a large enum
// can't blow the error message.
func joinEnumValues(enum []any) string {
	const maxValues = 12
	parts := make([]string, 0, len(enum))
	for i, e := range enum {
		if i >= maxValues {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%v", e))
	}
	return strings.Join(parts, ", ")
}
