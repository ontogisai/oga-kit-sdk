package streampipeline

import (
	"encoding/json"
	"strings"
)

// noProgress decides whether the ReAct loop should stop early before executing
// the next step (OGA-419, Property 3 — termination). Two conditions:
//
//  1. Duplicate action: the planner proposed a (tool, arguments) pair identical
//     to one it already executed this run. Repeating the same call cannot yield
//     new evidence.
//  2. K consecutive unproductive observations: the last `limit` executed steps
//     all failed, were skipped, or returned empty content. The planner is
//     thrashing; stop and assemble from what we have.
//
// `decided` is the set of steps already issued this run (carrying their args);
// `results` is the matching observation transcript; `next` is the step the
// planner just proposed (not yet appended). `limit <= 0` disables the
// consecutive-empties check.
func noProgress(decided []ToolPlanStep, results []ToolStepResult, next ToolPlanStep, limit int) bool {
	// 1. Exact duplicate action.
	for _, s := range decided {
		if s.ToolName == next.ToolName && sameArgs(s.Arguments, next.Arguments) {
			return true
		}
	}

	// 2. K consecutive unproductive observations.
	if limit > 0 && len(results) >= limit {
		empties := 0
		for i := len(results) - 1; i >= 0; i-- {
			r := results[i]
			if r.Skipped || !r.Success || strings.TrimSpace(r.Content) == "" {
				empties++
				if empties >= limit {
					return true
				}
				continue
			}
			break
		}
	}
	return false
}

// sameArgs reports whether two argument maps are equal by canonical JSON
// encoding. Order-insensitive (Go marshals map keys sorted) and tolerant of
// nil vs empty. Best-effort: on a marshal error it falls back to inequality so
// the loop is never stopped on a false positive.
func sameArgs(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(aj) == string(bj)
}
