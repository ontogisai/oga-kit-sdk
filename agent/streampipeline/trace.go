package streampipeline

import (
	"os"
	"strings"
)

// traceEnabled reports whether verbose agent-reasoning tracing is on.
//
// It is OFF by default and opt-in via the OGA_AGENT_TRACE environment variable
// (set to "1" or "true"). When enabled the pipeline logs, at Info level so the
// lines are visible without changing the slog level:
//
//   - the resolved grounding-step arguments — so the effective kg_* tool inputs
//     are observable (e.g. the exact kg_ts_read source_id that was queried,
//     which is the usual reason a time-series read comes back empty);
//   - the effective reasoning prompt (system + user) sent to the assembly LLM;
//   - the stream-collect outcome (chunk count + assembled artifact size).
//
// These lines can contain full prompts and tenant data, so they are gated
// behind the flag rather than emitted in normal operation. Enable for demos and
// debugging only.
//
// Wiring: for a locally-run agent (scripts/dev-kit.sh) add OGA_AGENT_TRACE=1 to
// the sidecar's env line. For a deployed sidecar container (make demo) the
// platform forwards OGA_AGENT_TRACE to the container when it is set on the
// deploying process's environment (see internal/sidecar Manager.buildEnv).
func traceEnabled() bool {
	v := os.Getenv("OGA_AGENT_TRACE")
	return v == "1" || strings.EqualFold(v, "true")
}

// proactiveReActLogEnabled reports whether per-event ReAct-loop logging on the
// stream→collect path (RunSync / RunText) is on (OGA-420 Gap 3). OFF by default,
// opt-in via OGA_PROACTIVE_REACT_LOG ("1"/"true"). When on, runArtifact logs the
// actual drained events (Thought, plan, tool_call, tool_result, usage, terminal
// status) at Info level so a proactive proposal's reasoning is reconstructable
// from logs without a UI consumer. Independent of OGA_AGENT_TRACE so the
// proactive loop can be traced without enabling full prompt tracing.
//
// Wiring mirrors OGA_AGENT_TRACE: set it on the deploying process's env and the
// platform forwards it to the sidecar container (internal/sidecar Manager.buildEnv).
func proactiveReActLogEnabled() bool {
	v := os.Getenv("OGA_PROACTIVE_REACT_LOG")
	return v == "1" || strings.EqualFold(v, "true")
}

// truncateForTrace bounds a string logged under trace so a runaway prompt or
// artifact cannot flood the log. The full value is emitted up to the cap; the
// suffix records how many bytes were elided.
func truncateForTrace(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…(" + itoa(len(s)-max) + " more bytes)"
}

// itoa is a tiny strconv.Itoa to avoid widening imports in trace.go.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
