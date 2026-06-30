// Package streampipeline — reactive clarification / confirm-before-write helpers
// (OGA-446). These support the input-required turn: deciding whether a tool
// mutates state, whether a pending confirmation authorises a mutating call, and
// synthesising the confirmation question the pipeline forces before any write.
package streampipeline

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// mutatingNameHints backstops tools whose schema omits an explicit Mutates flag
// (Requirement 3.2 fail-safe): a name that looks like a writer is treated as
// mutating even unannotated. Read-only tools (kg_search, kg_get_*, kg_query_*)
// fall through to non-mutating so best-effort retrieval is preserved.
var mutatingNameHints = []string{"create", "update", "delete", "write", "submit", "set_", "remove", "_create", "_update"}

// toolMutates reports whether calling toolName changes state. An explicit
// schema.Mutates wins; otherwise a conservative name heuristic applies so a
// writer that forgot to annotate still gets confirm-before-write.
func toolMutates(schemas map[string]agent.ToolSchema, toolName string) bool {
	if s, ok := schemas[toolName]; ok && s.Mutates != nil {
		return *s.Mutates
	}
	lower := strings.ToLower(toolName)
	for _, h := range mutatingNameHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// confirmationSatisfied reports whether an injected PendingConfirmation
// authorises executing toolName now. It requires BOTH that the token is an
// explicit CONFIRMATION (kind=confirmation) AND that its pending tool matches —
// so a disambiguation / missing_field pause (whose pending_tool is often the
// SAME write tool) does NOT silently authorise the write on the next turn. Only
// a turn answering an actual "shall I do X?" confirmation lets the write
// through; every other resume still hits the confirm-before-write interception.
// v1 matches on tool name only (not argument equality): the user may have
// amended details while confirming.
func confirmationSatisfied(pc *agent.ClarificationPayload, toolName string) bool {
	return pc != nil && pc.Kind == agent.ClarifyKindConfirmation && pc.PendingTool != "" && pc.PendingTool == toolName
}

// buildConfirmation synthesises the confirm-before-write question for a mutating
// step the planner tried to execute without a prior confirmation. The pipeline
// returns this as an input-required turn so no write happens until the user
// confirms (Property 3).
func buildConfirmation(step ToolPlanStep) *agent.ClarificationPayload {
	return &agent.ClarificationPayload{
		Kind:             agent.ClarifyKindConfirmation,
		Question:         fmt.Sprintf("I'm about to %s with %s. Reply \"yes\" to confirm, or tell me what to change.", step.ToolName, summarizeArgs(step.Arguments)),
		PendingTool:      step.ToolName,
		PartialArguments: step.Arguments,
	}
}

// summarizeArgs renders a stable, compact "k=v, k=v" view of tool arguments for
// the confirmation question. Keys are sorted for deterministic output.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "no arguments"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, args[k]))
	}
	return strings.Join(parts, ", ")
}

// resumeSeedFacts renders the pending_action_context into a planner seed block
// (OGA-446) so the resuming turn reasons over the paused question + the
// arguments gathered so far structurally — not only via injected chat history.
// Returns "" when there is no pending action (a fresh turn).
func resumeSeedFacts(pac *agent.ClarificationPayload) string {
	if pac == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("You previously paused this task to ask the user; their reply is the question/task above.\n")
	if pac.Question != "" {
		fmt.Fprintf(&b, "Your question was: %s\n", pac.Question)
	}
	if pac.PendingTool != "" {
		fmt.Fprintf(&b, "Pending action (the tool you intended to call once answered): %s\n", pac.PendingTool)
	}
	if len(pac.PartialArguments) > 0 {
		if js, err := json.Marshal(pac.PartialArguments); err == nil {
			fmt.Fprintf(&b, "Arguments gathered so far: %s\n", js)
		}
	}
	b.WriteString("Apply their reply to resolve the missing detail / disambiguation. " +
		"Confirm before writing if you have not already; once they confirm, proceed.")
	return b.String()
}
