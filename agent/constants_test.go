package agent

import (
	"strings"
	"testing"
)

// TestPlannerPromptTemplate_NoSensorFilter is the regression guard for
// OGA-314 Scope 3 (SDK-side mirror of the platform fix). The kg_ts_* tools'
// input schema names the filter field `source_filter`, but earlier drafts of
// the planner prompt told the LLM to use `sensor_filter`. The LLM faithfully
// followed the prompt and every kg_ts_analyze plan with KG-based source
// discovery failed at the handler with `OGA-CORE-VAL-1001 field source_id or
// source_filter is required`.
//
// Keep this test as a tripwire: any future edit that reintroduces
// `sensor_filter` in the SDK planner prompt fails CI immediately.
func TestPlannerPromptTemplate_NoSensorFilter(t *testing.T) {
	if strings.Contains(PlannerPromptTemplate, "sensor_filter") {
		t.Fatal("SDK planner prompt contains 'sensor_filter' — the kg_ts_* tools expect 'source_filter' (matches the platform-side schema in oga-platform/internal/mcptoolserver/register_tier1.go). Reverting introduces OGA-CORE-VAL-1001 errors at the handler.")
	}
}

// TestPlannerPromptTemplate_DocumentsSourceFilter ensures both kg_ts_read and
// kg_ts_analyze prompt blocks reference source_filter. Without explicit
// guidance the LLM defaults to source_id only, missing KG-based discovery.
func TestPlannerPromptTemplate_DocumentsSourceFilter(t *testing.T) {
	if !strings.Contains(PlannerPromptTemplate, "source_filter") {
		t.Fatal("SDK planner prompt MUST mention source_filter so the LLM knows it can discover sources via KG relationships instead of supplying source_id directly")
	}

	for _, tool := range []string{"kg_ts_read", "kg_ts_analyze"} {
		idx := strings.Index(PlannerPromptTemplate, tool)
		if idx < 0 {
			t.Errorf("SDK planner prompt does not mention %s", tool)
			continue
		}
		// Look for source_filter within the next ~400 chars of the tool name —
		// roughly the param block. Scope keeps the test specific.
		windowEnd := idx + 400
		if windowEnd > len(PlannerPromptTemplate) {
			windowEnd = len(PlannerPromptTemplate)
		}
		window := PlannerPromptTemplate[idx:windowEnd]
		if !strings.Contains(window, "source_filter") {
			t.Errorf("SDK planner prompt block for %s does not mention source_filter — LLM will default to source_id only and miss KG discovery", tool)
		}
	}
}

// TestPlannerPromptTemplate_MetricRequired is the OGA-387 regression guard:
// the planner template MUST mandate the `metric` argument for the kg_ts_*
// tools and warn against a source_filter that binds nothing (only max_sources).
// Without this, the planner LLM omits `metric` and the tools reject the call
// with OGA-CORE-VAL-1001 (the failure traced on the chiller investigation
// route).
func TestPlannerPromptTemplate_MetricRequired(t *testing.T) {
	if !strings.Contains(PlannerPromptTemplate, "metric (REQUIRED)") {
		t.Error("planner template must mark `metric` as REQUIRED for kg_ts_read/kg_ts_analyze (OGA-387)")
	}
	if !strings.Contains(PlannerPromptTemplate, "OGA-CORE-VAL-1001") {
		t.Error("planner template should name the validation error the ts tools raise when metric/source is missing")
	}
	// Must warn against the exact bad shape seen in the trace: a source_filter
	// with only max_sources and no related_to/entity_type.
	if !strings.Contains(PlannerPromptTemplate, "max_sources") ||
		!strings.Contains(PlannerPromptTemplate, "related_to") {
		t.Error("planner template should warn that a source_filter must bind a source via related_to/entity_type, not only max_sources")
	}
}
