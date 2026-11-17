package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingRuntime counts the messages that reach the runtime.
type countingRuntime struct {
	stubRuntime
	handled atomic.Int32
}

func (c *countingRuntime) HandleMessage(ctx context.Context, msg *A2AMessage) (*A2AResponse, error) {
	c.handled.Add(1)
	return c.stubRuntime.HandleMessage(ctx, msg)
}

// TestAgentMux_AnswersOnlyItsOwnTenant (OGA-1259): a kit agent serves one
// tenant and answers from that tenant's data with that tenant's workload
// token, so a message that does not name its tenant in X-Tenant-ID is refused
// before it reaches the runtime, on both message routes. The card and the
// probes stay open.
func TestAgentMux_AnswersOnlyItsOwnTenant(t *testing.T) {
	rt := &countingRuntime{stubRuntime: stubRuntime{card: &AgentCard{Name: "Test"}, reply: "echo", healthy: true, ready: true}}
	srv := httptest.NewServer(newAgentMux(rt, "acme"))
	defer srv.Close()

	const body = `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	post := func(path string, tenants ...string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		for _, tenant := range tenants {
			req.Header.Add(HeaderTenantID, tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	for _, path := range []string{"/", "/agents/fm-operations-agent"} {
		for name, tenants := range map[string][]string{
			"no tenant":         nil,
			"blank tenant":      {" "},
			"another tenant":    {"beta"},
			"two tenants":       {"acme", "beta"},
			"case-changed slug": {"ACME"},
		} {
			resp := post(path, tenants...)
			var out struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("%s %s: decode: %v", path, name, err)
			}
			if resp.StatusCode != http.StatusForbidden || out.Error.Code != jsonRPCErrTenant {
				t.Errorf("%s %s: %d code %d, want 403 code %d", path, name, resp.StatusCode, out.Error.Code, jsonRPCErrTenant)
			}
		}
	}
	if n := rt.handled.Load(); n != 0 {
		t.Fatalf("%d refused messages reached the runtime", n)
	}

	for _, tenants := range [][]string{{"acme"}, {"acme", "acme"}} {
		if resp := post("/agents/fm-operations-agent", tenants...); resp.StatusCode != http.StatusOK {
			t.Errorf("X-Tenant-ID %v: %d, want 200", tenants, resp.StatusCode)
		}
	}
	if n := rt.handled.Load(); n != 2 {
		t.Errorf("own-tenant messages handled = %d, want 2", n)
	}

	for _, path := range []string{"/.well-known/agent-card.json", "/agents/fm-operations-agent/.well-known/agent-card.json", "/healthz"} {
		resp, err := http.Get(srv.URL + path) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestAgentMux_NoTenantRefusesEveryMessage: an agent started without
// OGA_TENANT_ID cannot tell its tenant from another, so it answers no message.
func TestAgentMux_NoTenantRefusesEveryMessage(t *testing.T) {
	rt := &countingRuntime{stubRuntime: stubRuntime{reply: "echo"}}
	srv := httptest.NewServer(newAgentMux(rt, ""))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(HeaderTenantID, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || rt.handled.Load() != 0 {
		t.Errorf("status %d, handled %d; want 403 and none", resp.StatusCode, rt.handled.Load())
	}
}
