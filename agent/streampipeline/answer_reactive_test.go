package streampipeline

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// A synchronous reactive question (message/send) is answered by the ReAct
// loop: the model's chosen tool runs with exactly the arguments it planned
// (no _prior_result, OGA-1189), a kit write runs without an input-required
// pause (this route carries no resume token back), and the reply is the
// assembled answer.
func TestAnswerReactive_RunsReActLoop(t *testing.T) {
	gw := &fakeGateway{
		tools:        map[string]json.RawMessage{"fm_create_work_order": json.RawMessage(`{"id":"wo-1"}`)},
		streamChunks: []string{"Work order ", "wo-1 created."},
	}
	// The planner asks for the write until it has run, then finalizes.
	gw.completionFn = func(_ context.Context, _ *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
		content := `{"thought":"done","final":true}`
		if len(gw.callToolCalls) == 0 {
			content = `{"thought":"create it","action":{"tool":"fm_create_work_order","arguments":{"asset_id":"a-1"}}}`
		}
		return &gateway.ChatCompletionResponse{Choices: []gateway.ChatChoice{
			{Message: gateway.ChatMessage{Role: "assistant", Content: content}},
		}}, nil
	}
	rt := newTestRuntime(t, &agent.DomainAgentProfile{
		Name:         "FM Agent",
		AgentID:      "fm-agent",
		Capabilities: []agent.CapabilityDef{{Name: "work_orders", Tools: []string{"fm_create_work_order"}}},
	})
	msg := &agent.A2AMessage{Method: "message/send", Params: &agent.MessageParams{Message: &agent.Message{
		Role:  "user",
		Parts: []agent.Part{{Text: "create a work order for asset a-1"}},
	}}}

	resp, err := answerReactive(context.Background(), gw, rt, msg, newToolSchemaCache())
	if err != nil {
		t.Fatalf("answerReactive: %v", err)
	}

	if len(gw.callToolCalls) != 1 || gw.callToolCalls[0].Tool != "fm_create_work_order" {
		t.Fatalf("expected the write to run once without a confirmation pause, got %v", gw.callToolCalls)
	}
	want := map[string]any{"asset_id": "a-1"}
	if got, ok := gw.callToolCalls[0].Params.(map[string]any); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("tool arguments = %#v, want exactly the planned %v", gw.callToolCalls[0].Params, want)
	}
	if resp == nil || resp.Message == nil || len(resp.Message.Parts) != 1 {
		t.Fatalf("expected one agent message part, got %+v", resp)
	}
	if resp.Message.Role != "agent" || !strings.Contains(resp.Message.Parts[0].Text, "wo-1 created.") {
		t.Errorf("reply = %q (role %q), want the assembled answer", resp.Message.Parts[0].Text, resp.Message.Role)
	}
}

// A message with no text is refused before any model call, as before.
func TestAnswerReactive_NoText(t *testing.T) {
	gw := &fakeGateway{}
	rt := newTestRuntime(t, &agent.DomainAgentProfile{Name: "FM Agent", AgentID: "fm-agent"})
	msg := &agent.A2AMessage{Params: &agent.MessageParams{Message: &agent.Message{Role: "user"}}}

	if _, err := answerReactive(context.Background(), gw, rt, msg, newToolSchemaCache()); err == nil {
		t.Fatal("expected an error for a message with no text")
	}
	if gw.completionCalls != 0 || len(gw.callToolCalls) != 0 {
		t.Errorf("no model or tool call may happen, got %d completions, %v tools", gw.completionCalls, gw.callToolCalls)
	}
}
