package streampipeline

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// actionDecision mirrors the proactive action-decision shape: reasoning is a
// STRING (not an array) — the schema constraint that drives OGA-423 Gap 2B.
type actionDecision struct {
	ActionType string `json:"action_type"`
	Reasoning  string `json:"reasoning"`
}

func actionDecisionSchema(t *testing.T) any {
	return map[string]any{
		"type":     "object",
		"required": []any{"action_type", "reasoning"},
		"properties": map[string]any{
			"action_type": map[string]any{"type": "string"},
			"reasoning":   map[string]any{"type": "string"},
		},
	}
}

// oneToolPlanner yields exactly one tool step on turn 0, then Done. Used to
// prove the ReAct loop (and therefore the single tool call) runs exactly once
// across a schema-validation retry.
func oneToolPlanner() *scriptedPlanner {
	return &scriptedPlanner{steps: []ToolPlanStep{{Name: "s0", ToolName: "kg_get_entity", Arguments: map[string]any{"id": "e1"}}}}
}

// TestRunSync_SchemaRetry_DoesNotReRunTools is the OGA-423 Gap 2A acceptance
// test: when attempt 1's assembled output fails schema validation, RunSync
// re-issues ONLY the assembly call against the already-gathered transcript — it
// must NOT re-run the ReAct loop or re-execute any tool.
func TestRunSync_SchemaRetry_DoesNotReRunTools(t *testing.T) {
	schema := compile(t, actionDecisionSchema(t).(map[string]any))
	gw := &fakeGateway{
		streamChunksSeq: [][]string{
			{`{"action_type":"create"}`},                       // attempt 1: missing reasoning → invalid
			{`{"action_type":"create","reasoning":"because"}`}, // attempt 2: valid
		},
	}
	deps := Deps{Gateway: gw, Config: DefaultConfig()}

	out, _, err := RunSync[actionDecision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "a"}}, oneToolPlanner(), schema)
	if err != nil {
		t.Fatalf("RunSync after valid retry: %v", err)
	}
	if out.ActionType != "create" || out.Reasoning != "because" {
		t.Errorf("decoded = %+v, want {create, because}", out)
	}

	// THE acceptance criterion: the tool ran exactly once despite the retry.
	if got := len(gw.callToolCalls); got != 1 {
		t.Errorf("tool calls = %d, want 1 (tools must NOT be re-run on schema retry)", got)
	}
	// Two assembly attempts (the only LLM calls — the planner is scripted).
	if got := len(gw.chatReqs); got != 2 {
		t.Errorf("assembly calls = %d, want 2 (gather once, assemble twice)", got)
	}
}

// TestRunSync_NoRetry_RunsToolsAndAssemblesOnce confirms the happy path is
// unchanged: a first-attempt-valid output runs the tool once and assembles once.
func TestRunSync_NoRetry_RunsToolsAndAssemblesOnce(t *testing.T) {
	schema := compile(t, actionDecisionSchema(t).(map[string]any))
	gw := &fakeGateway{streamChunks: []string{`{"action_type":"create","reasoning":"ok"}`}}
	deps := Deps{Gateway: gw, Config: DefaultConfig()}

	_, _, err := RunSync[actionDecision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "a"}}, oneToolPlanner(), schema)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if got := len(gw.callToolCalls); got != 1 {
		t.Errorf("tool calls = %d, want 1", got)
	}
	if got := len(gw.chatReqs); got != 1 {
		t.Errorf("assembly calls = %d, want 1 (no retry on valid first attempt)", got)
	}
}

// TestRunSync_ReasoningArrayCoercion is the OGA-423 Gap 2B acceptance test: an
// action decision whose `reasoning` is emitted as a JSON array of strings
// validates on the FIRST attempt (no retry) because validateAndUnmarshal coerces
// it into a newline-joined string.
func TestRunSync_ReasoningArrayCoercion(t *testing.T) {
	schema := compile(t, actionDecisionSchema(t).(map[string]any))
	gw := &fakeGateway{
		streamChunks: []string{`{"action_type":"flag","reasoning":["a","b","c"]}`},
	}
	deps := Deps{Gateway: gw, Config: DefaultConfig()}

	out, _, err := RunSync[actionDecision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "a"}}, oneToolPlanner(), schema)
	if err != nil {
		t.Fatalf("RunSync with array reasoning: %v", err)
	}
	if out.Reasoning != "a\nb\nc" {
		t.Errorf("reasoning = %q, want %q", out.Reasoning, "a\nb\nc")
	}
	// Coercion → valid on attempt 1 → exactly one assembly call (no retry).
	if got := len(gw.chatReqs); got != 1 {
		t.Errorf("assembly calls = %d, want 1 (array reasoning must validate on first attempt)", got)
	}
}

// TestCoerceReasoningArray covers the helper's edge cases directly.
func TestCoerceReasoningArray(t *testing.T) {
	t.Run("array of strings is joined", func(t *testing.T) {
		m := map[string]any{"reasoning": []any{"x", "y"}}
		if !coerceReasoningArray(m) {
			t.Fatal("expected coercion to report a change")
		}
		if m["reasoning"] != "x\ny" {
			t.Errorf("reasoning = %v, want %q", m["reasoning"], "x\ny")
		}
	})
	t.Run("string is untouched", func(t *testing.T) {
		m := map[string]any{"reasoning": "already a string"}
		if coerceReasoningArray(m) {
			t.Error("expected no change for a string reasoning")
		}
	})
	t.Run("mixed array is left untouched", func(t *testing.T) {
		m := map[string]any{"reasoning": []any{"x", 7}}
		if coerceReasoningArray(m) {
			t.Error("expected no change for a non-all-string array")
		}
	})
	t.Run("absent is a no-op", func(t *testing.T) {
		m := map[string]any{"other": 1}
		if coerceReasoningArray(m) {
			t.Error("expected no change when reasoning absent")
		}
	})
}

// TestRunSync_PerTurnReactLog_UnderAgentTrace is the OGA-423 Gap 1 acceptance
// test: with OGA_AGENT_TRACE=1, the synchronous (proactive) path logs the
// turn-by-turn ReAct events — Thought / action / observation — not just the
// final assembly prompt.
func TestRunSync_PerTurnReactLog_UnderAgentTrace(t *testing.T) {
	t.Setenv("OGA_AGENT_TRACE", "1")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	schema := compile(t, actionDecisionSchema(t).(map[string]any))
	gw := &fakeGateway{streamChunks: []string{`{"action_type":"create","reasoning":"ok"}`}}
	deps := Deps{Gateway: gw, Config: DefaultConfig(), Logger: logger}

	planner := &scriptedPlanner{
		steps:     []ToolPlanStep{{Name: "s0", ToolName: "kg_get_entity", Arguments: map[string]any{"id": "e1"}}},
		narrative: "I should fetch the entity first.",
	}

	_, _, err := RunSync[actionDecision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "fm-operations"}}, planner, schema)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	logs := buf.String()
	for _, want := range []string{"react: thought", "react: action", "react: observation"} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected per-turn log %q under OGA_AGENT_TRACE; logs:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, "kg_get_entity") {
		t.Errorf("expected the tool name in the react logs; logs:\n%s", logs)
	}
}

// TestRunSync_NoReactLog_WhenTraceOff confirms the per-turn logging stays off by
// default (no log spam in steady state).
func TestRunSync_NoReactLog_WhenTraceOff(t *testing.T) {
	t.Setenv("OGA_AGENT_TRACE", "")
	t.Setenv("OGA_PROACTIVE_REACT_LOG", "")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	schema := compile(t, actionDecisionSchema(t).(map[string]any))
	gw := &fakeGateway{streamChunks: []string{`{"action_type":"create","reasoning":"ok"}`}}
	deps := Deps{Gateway: gw, Config: DefaultConfig(), Logger: logger}

	_, _, err := RunSync[actionDecision](
		context.Background(), NewPipeline(), deps,
		Input{Query: "q", Actor: agent.EventActor{ID: "a"}}, oneToolPlanner(), schema)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if strings.Contains(buf.String(), "react: ") {
		t.Errorf("did not expect react logs when both trace flags are off; logs:\n%s", buf.String())
	}
}
