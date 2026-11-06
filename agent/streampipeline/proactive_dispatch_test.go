package streampipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// dispatchGateway is an httptest Platform Gateway for the message/send
// dispatch test. It serves tools/list, tools/call and chat completions (a
// non-streaming request gets one ChatCompletionResponse; a streaming one gets
// concatenated JSON ChatChunk objects, which is what the client decodes), and
// records the system prompt of every chat request and every tool call.
type dispatchGateway struct {
	mu        sync.Mutex
	systems   []string
	toolCalls []struct {
		Tool   string         `json:"tool"`
		Params map[string]any `json:"params"`
	}
}

func (g *dispatchGateway) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/tools/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tools":[{"name":"fm_get_asset","description":"Get an asset.",` +
			`"inputSchema":{"type":"object","properties":{"asset_id":{"type":"string"}},"required":["asset_id"]},` +
			`"mutates":false}]}`))
	})
	mux.HandleFunc("/mcp/tools/call", func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Tool   string         `json:"tool"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Errorf("decode tools/call: %v", err)
		}
		g.mu.Lock()
		g.toolCalls = append(g.toolCalls, call)
		g.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"a-1","status":"running"}`))
	})
	mux.HandleFunc("/llm/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []gateway.ChatMessage `json:"messages"`
			Stream   bool                  `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode chat request: %v", err)
		}
		g.mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "system" {
				g.systems = append(g.systems, m.Content)
			}
		}
		called := len(g.toolCalls) > 0
		g.mu.Unlock()

		if req.Stream {
			_, _ = w.Write([]byte(`{"choices":[{"index":0,"delta":{"content":"Asset a-1 is running."}}]}`))
			return
		}
		decision := `{"thought":"done","final":true}`
		if !called {
			decision = `{"thought":"look it up","action":{"tool":"fm_get_asset","arguments":{"asset_id":"a-1"}}}`
		}
		_ = json.NewEncoder(w).Encode(gateway.ChatCompletionResponse{Choices: []gateway.ChatChoice{
			{Message: gateway.ChatMessage{Role: "assistant", Content: decision}},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A message/send question to a kit that wires NewProactiveMessageHandler (as
// every kit's main.go does) is answered by the ReAct loop, not by the one-shot
// PlanAndExecute (OGA-1189): the model gets the next-step decision prompt and
// never the tool-planning prompt, the tool runs with exactly the planned
// arguments, and the reply is the streamed answer. Driven through
// DefaultRuntime.HandleMessage and the real gateway client, so it fails if the
// handler sends a reactive message back to DefaultRuntime.HandleReactive.
func TestProactiveMessageHandler_MessageSendRunsReAct(t *testing.T) {
	gw := &dispatchGateway{}
	srv := gw.serve(t)
	deps, err := agent.ConnectRuntimeDeps(context.Background(), &agent.RuntimeDepsConfig{
		GatewayURL: srv.URL,
		TenantID:   "test-tenant",
		AgentID:    "fm-agent",
	})
	if err != nil {
		t.Fatalf("ConnectRuntimeDeps: %v", err)
	}
	t.Cleanup(deps.Close)
	rt := agent.NewDefaultRuntime(&agent.DomainAgentProfile{
		Name:         "FM Agent",
		AgentID:      "fm-agent",
		Capabilities: []agent.CapabilityDef{{Name: "assets", Tools: []string{"fm_get_asset"}}},
	}, deps, agent.WithMessageHandler(NewProactiveMessageHandler()))

	resp, err := rt.HandleMessage(context.Background(), &agent.A2AMessage{
		Method: "message/send",
		Params: &agent.MessageParams{Message: &agent.Message{
			Role:  "user",
			Parts: []agent.Part{{Text: "is asset a-1 running?"}},
		}},
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	plannerPrompt, _, _ := strings.Cut(strings.TrimPrefix(agent.PlannerPromptTemplate, "%s"), "\n")
	decided := false
	for _, s := range gw.systems {
		if strings.Contains(s, plannerPrompt) {
			t.Fatalf("message/send reached the one-shot planner (system prompt %q)", plannerPrompt)
		}
		if strings.Contains(s, "Decide the SINGLE next action") {
			decided = true
		}
	}
	if !decided {
		t.Errorf("no ReAct decision prompt was sent; system prompts: %q", gw.systems)
	}
	if len(gw.toolCalls) != 1 || gw.toolCalls[0].Tool != "fm_get_asset" {
		t.Fatalf("tool calls = %+v, want one fm_get_asset call", gw.toolCalls)
	}
	if want := map[string]any{"asset_id": "a-1"}; !reflect.DeepEqual(gw.toolCalls[0].Params, want) {
		t.Errorf("tool params = %v, want exactly the planned %v", gw.toolCalls[0].Params, want)
	}
	if resp == nil || resp.Message == nil || len(resp.Message.Parts) != 1 ||
		!strings.Contains(resp.Message.Parts[0].Text, "Asset a-1 is running.") {
		t.Errorf("reply = %+v, want the streamed answer", resp)
	}
}
