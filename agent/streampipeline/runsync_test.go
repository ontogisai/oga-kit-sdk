package streampipeline

import (
	"context"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
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

// TestRunSyncWithUsage_AggregatesUsage verifies the proactive-path entry point
// returns the per-request token aggregate (OGA-420 Gap 4). A no-step plan goes
// straight to assembly; the fake reports usage on the assembly stream, so the
// aggregate equals that usage and is available.
func TestRunSyncWithUsage_AggregatesUsage(t *testing.T) {
	schema := compile(t, map[string]any{
		"type":     "object",
		"required": []any{"action_type", "reason"},
		"properties": map[string]any{
			"action_type": map[string]any{"type": "string"},
			"reason":      map[string]any{"type": "string"},
		},
	})
	gw := &fakeGateway{
		streamChunks: []string{`{"action_type":"create","reason":"x"}`},
		streamUsage:  &gateway.Usage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
	}
	planner := &scriptedPlanner{} // Done on turn 0 → straight to assembly
	deps := Deps{Gateway: gw, Config: DefaultConfig()}

	out, _, usage, avail, err := RunSyncWithUsage[decision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "a"}}, planner, schema)
	if err != nil {
		t.Fatalf("RunSyncWithUsage: %v", err)
	}
	if out.ActionType != "create" {
		t.Errorf("decoded = %+v, want action_type=create", out)
	}
	if !avail || usage.TotalTokens != 100 {
		t.Errorf("usage = %+v (avail=%v), want total 100 available", usage, avail)
	}
}

// TestRunSync_StillWorks verifies the back-compat wrapper delegates correctly.
func TestRunSync_StillWorks(t *testing.T) {
	schema := compile(t, map[string]any{
		"type":     "object",
		"required": []any{"action_type", "reason"},
		"properties": map[string]any{
			"action_type": map[string]any{"type": "string"},
			"reason":      map[string]any{"type": "string"},
		},
	})
	gw := &fakeGateway{streamChunks: []string{`{"action_type":"create","reason":"x"}`}}
	deps := Deps{Gateway: gw, Config: DefaultConfig()}

	out, _, err := RunSync[decision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "a"}}, &scriptedPlanner{}, schema)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if out.ActionType != "create" {
		t.Errorf("decoded = %+v, want action_type=create", out)
	}
}
