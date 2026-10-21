package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// newTestRuntime builds a runtime with a throwaway gateway client (the tests
// here exercise the JSON-RPC transport, not outbound gateway calls).
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	gw := gateway.NewPlatformGatewayClient("http://localhost:8050", "tnt1")
	return NewRuntime(gw, nil)
}

func TestRuntime_RegisterRejectsBadInput(t *testing.T) {
	rt := newTestRuntime(t)
	if err := rt.RegisterFunc("", "d", nil, func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil }); err == nil {
		t.Error("empty name must be rejected")
	}
	if err := rt.RegisterFunc("t", "d", nil, nil); err == nil {
		t.Error("nil handler must be rejected")
	}
	ok := func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil }
	if err := rt.RegisterFunc("dup", "d", nil, ok); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := rt.RegisterFunc("dup", "d", nil, ok); err == nil {
		t.Error("duplicate name must be rejected")
	}
}

func TestRuntime_ToolsListAndCall(t *testing.T) {
	rt := newTestRuntime(t)
	_ = rt.RegisterFunc("fm_echo", "echoes the tenant + args",
		json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		func(ctx context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"tenant": TenantFromContext(ctx), "args": args}, nil
		})

	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	// tools/list
	listResp := rpc(t, srv.URL, `{"jsonrpc":"2.0","id":"1","method":"tools/list"}`, nil)
	tools, _ := listResp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "fm_echo" {
		t.Fatalf("tools/list = %v, want [fm_echo]", listResp["result"])
	}

	// tools/call with X-Tenant-ID propagation
	callResp := rpc(t, srv.URL,
		`{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"fm_echo","arguments":{"x":"hi"}}}`,
		map[string]string{"X-Tenant-ID": "tnt1"})
	inner := unwrapContent(t, callResp)
	if inner["tenant"] != "tnt1" {
		t.Errorf("handler did not see X-Tenant-ID; got %v", inner["tenant"])
	}
}

func TestRuntime_ToolErrorIsIsErrorResult(t *testing.T) {
	rt := newTestRuntime(t)
	_ = rt.RegisterFunc("fm_fail", "always fails", nil,
		func(_ context.Context, _ json.RawMessage) (any, error) {
			return nil, errBoom
		})
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	resp := rpc(t, srv.URL, `{"jsonrpc":"2.0","id":"3","method":"tools/call","params":{"name":"fm_fail"}}`, nil)
	if resp["error"] != nil {
		t.Fatalf("tool error must NOT be a JSON-RPC transport error; got %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true result envelope; got %v", result)
	}
}

func TestRuntime_UnknownToolAndBadRequest(t *testing.T) {
	rt := newTestRuntime(t)
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	// Unknown tool → JSON-RPC invalid params error.
	resp := rpc(t, srv.URL, `{"jsonrpc":"2.0","id":"4","method":"tools/call","params":{"name":"nope"}}`, nil)
	if resp["error"] == nil {
		t.Error("unknown tool must return a JSON-RPC error")
	}

	// Wrong jsonrpc version → invalid request.
	resp = rpc(t, srv.URL, `{"jsonrpc":"1.0","id":"5","method":"ping"}`, nil)
	if resp["error"] == nil {
		t.Error("jsonrpc != 2.0 must return a JSON-RPC error")
	}

	// ping → empty result.
	resp = rpc(t, srv.URL, `{"jsonrpc":"2.0","id":"6","method":"ping"}`, nil)
	if resp["error"] != nil {
		t.Errorf("ping should succeed; got %v", resp["error"])
	}
}

func TestRuntime_HealthAndReady(t *testing.T) {
	rt := newTestRuntime(t)
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path) //nolint:noctx // short test probe
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
	}
}

// A tool's handler is held to its own Composition (OGA-1121): its gateway call
// to a composed tool is sent, one to any other tool fails as an isError result
// and is never sent, even when another tool of the same sidecar composes it.
func TestRuntime_HandlerHeldToItsComposition(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []string
	)
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tool string `json:"tool"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sent = append(sent, body.Tool)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer gwSrv.Close()
	rt := NewRuntime(gateway.NewPlatformGatewayClient(gwSrv.URL, "tnt1"), nil)

	callTool := func(name string) ToolHandler {
		return func(ctx context.Context, _ json.RawMessage) (any, error) {
			if _, err := rt.Gateway().CallTool(ctx, name, map[string]any{}); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		}
	}
	for _, tool := range []Tool{
		{Name: "fm_read", Description: "reads", Composition: []string{"kg_get_entity"}, Handler: callTool("kg_get_entity")},
		{Name: "fm_drifted", Description: "writes without declaring it", Composition: []string{"kg_get_entity"}, Handler: callTool("kg_create_entity")},
		{Name: "fm_write", Description: "writes", Composition: []string{"kg_create_entity"}, Handler: callTool("kg_create_entity")},
	} {
		if err := rt.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()
	call := func(name string) map[string]any {
		return rpc(t, srv.URL, `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"`+name+`"}}`, nil)
	}

	if inner := unwrapContent(t, call("fm_read")); inner["ok"] != true {
		t.Errorf("fm_read: %v", inner)
	}
	resp := call("fm_drifted")
	if result, _ := resp["result"].(map[string]any); result["isError"] != true {
		t.Errorf("fm_drifted: a call outside its composition succeeded: %v", resp)
	}
	if inner := unwrapContent(t, call("fm_write")); inner["ok"] != true {
		t.Errorf("fm_write: %v", inner)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"kg_get_entity", "kg_create_entity"}; len(sent) != 2 || sent[0] != want[0] || sent[1] != want[1] {
		t.Errorf("gateway received %v, want %v (the drifted call must not be sent)", sent, want)
	}
}

// --- helpers ---

var errBoom = boomError("boom")

type boomError string

func (e boomError) Error() string { return string(e) }

func rpc(t *testing.T, url, body string, headers map[string]string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/mcp", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// unwrapContent pulls the inner JSON object out of the MCP content envelope of
// a successful tools/call response.
func unwrapContent(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object; got %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content array; got %v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var inner map[string]any
	if err := json.Unmarshal([]byte(text), &inner); err != nil {
		t.Fatalf("decode inner content %q: %v", text, err)
	}
	return inner
}
