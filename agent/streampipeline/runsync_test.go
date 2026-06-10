package streampipeline

import (
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compile(t *testing.T, doc map[string]any) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	const u = "mem://test.json"
	if err := c.AddResource(u, doc); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	sch, err := c.Compile(u)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return sch
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose before {\"a\":1} prose after", `{"a":1}`},
		{"no object here", ""},
	}
	for _, tc := range cases {
		if got := extractJSONObject(tc.in); got != tc.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

type decision struct {
	ActionType string `json:"action_type"`
	Reason     string `json:"reason"`
}

func TestValidateAndUnmarshal(t *testing.T) {
	schema := compile(t, map[string]any{
		"type":     "object",
		"required": []any{"action_type", "reason"},
		"properties": map[string]any{
			"action_type": map[string]any{"type": "string"},
			"reason":      map[string]any{"type": "string"},
		},
	})

	t.Run("valid", func(t *testing.T) {
		got, err := validateAndUnmarshal[decision](`{"action_type":"create","reason":"because"}`, schema)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ActionType != "create" || got.Reason != "because" {
			t.Errorf("unexpected: %+v", got)
		}
	})

	t.Run("schema violation", func(t *testing.T) {
		_, err := validateAndUnmarshal[decision](`{"action_type":"create"}`, schema)
		if err == nil {
			t.Fatal("expected schema validation error (missing reason)")
		}
	})

	t.Run("no json object", func(t *testing.T) {
		_, err := validateAndUnmarshal[decision]("just prose", schema)
		if err == nil {
			t.Fatal("expected error when no JSON object present")
		}
	})
}
