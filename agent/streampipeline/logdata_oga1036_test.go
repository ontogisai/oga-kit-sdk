package streampipeline

import (
	"strings"
	"testing"
)

// OGA-1036: a schema failure's message is logged at WARN and returned into an
// ERROR log. The library quotes the offending instance value for pattern and
// format failures, and that value is LLM output grounded on tenant data. The
// summary keeps the location and the failing keyword only.
func TestSchemaErrorSummary_OmitsInstanceValues(t *testing.T) {
	sch := compile(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"work_order_ref": map[string]any{"type": "string", "pattern": "^WO-[0-9]+$"},
			"priority":       map[string]any{"enum": []any{"low", "high"}},
		},
	})
	const secretValue = "Call Jane Doe on 91234567 about AHU-3"
	_, err := validateAndUnmarshal[map[string]any](`{"work_order_ref": "`+secretValue+`", "priority": "urgent"}`, sch)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	msg := err.Error()
	for _, leak := range []string{secretValue, "Jane", "91234567", "urgent"} {
		if strings.Contains(msg, leak) {
			t.Errorf("validation error carries instance data %q: %q", leak, msg)
		}
	}
	for _, want := range []string{"/work_order_ref: pattern", "/priority: enum"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary %q lacks %q", msg, want)
		}
	}
}
