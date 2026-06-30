package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubRuntime is a minimal AgentRuntime for testing the HTTP layer.
type stubRuntime struct {
	card    *AgentCard
	reply   string
	healthy bool
	ready   bool
}

func (s *stubRuntime) ServeAgentCard() *AgentCard { return s.card }
func (s *stubRuntime) HandleMessage(_ context.Context, msg *A2AMessage) (*A2AResponse, error) {
	text := ExtractText(msg.Params.Message.Parts)
	return &A2AResponse{
		Message: &Message{
			Role:  "agent",
			Parts: []Part{{Text: fmt.Sprintf("%s: %s", s.reply, text)}},
		},
	}, nil
}
func (s *stubRuntime) HandleStream(_ context.Context, _ *A2AMessage, stream StreamWriter) error {
	_ = stream.WriteEvent(context.Background(), &StreamEvent{
		Type:    EventTypeArtifact,
		Payload: &ArtifactPayload{Parts: []ArtifactPart{{Text: s.reply}}},
	})
	return stream.Close()
}
func (s *stubRuntime) Healthz(_ context.Context) error {
	if !s.healthy {
		return fmt.Errorf("unhealthy")
	}
	return nil
}
func (s *stubRuntime) Readyz(_ context.Context) error {
	if !s.ready {
		return fmt.Errorf("not ready")
	}
	return nil
}

func newTestServer(rt AgentRuntime) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", agentCardHandler(rt))
	mux.HandleFunc("POST /", messageHandlerFunc(rt))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := rt.Healthz(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := rt.Readyz(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

func TestAgentCard(t *testing.T) {
	rt := &stubRuntime{
		card: &AgentCard{
			Name:               "Test Agent",
			Description:        "A test agent",
			URL:                "http://localhost:8200",
			Version:            "1.0.0",
			Capabilities:       map[string]any{},
			DefaultInputModes:  []string{"text/plain"},
			DefaultOutputModes: []string{"text/plain"},
			Skills: []Skill{
				{ID: "test", Name: "Test Skill", Description: "Does testing"},
			},
			SupportedInterfaces: []SupportedInterface{
				{URL: "http://localhost:8200", ProtocolBinding: "JSONRPC", ProtocolVersion: "1.0"},
			},
		},
		healthy: true,
		ready:   true,
	}

	srv := newTestServer(rt)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}

	if card.Name != "Test Agent" {
		t.Errorf("card.Name = %q, want %q", card.Name, "Test Agent")
	}
	if len(card.Skills) != 1 {
		t.Errorf("card.Skills count = %d, want 1", len(card.Skills))
	}
}

func TestMessageSend(t *testing.T) {
	rt := &stubRuntime{
		card:    &AgentCard{Name: "Test"},
		reply:   "echo",
		healthy: true,
		ready:   true,
	}

	srv := newTestServer(rt)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hello"}]}}}`
	resp, err := http.Post(srv.URL+"/", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST message: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Check JSON-RPC structure
	if result["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", result["jsonrpc"])
	}

	// Check result contains message
	res, ok := result["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", result["result"])
	}
	msg, ok := res["message"].(map[string]any)
	if !ok {
		t.Fatalf("result.message is not a map: %T", res["message"])
	}
	if msg["role"] != "agent" {
		t.Errorf("role = %v, want agent", msg["role"])
	}
}

func TestMessageSend_NumericID(t *testing.T) {
	rt := &stubRuntime{
		card:    &AgentCard{Name: "Test"},
		reply:   "ok",
		healthy: true,
		ready:   true,
	}

	srv := newTestServer(rt)
	defer srv.Close()

	// Test with numeric ID (as sent by agentgateway Playground)
	body := `{"jsonrpc":"2.0","id":42,"method":"message/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}`
	resp, err := http.Post(srv.URL+"/", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// ID should be echoed back as number
	if result["id"] == nil {
		t.Error("response id is nil")
	}
}

func TestHealthz(t *testing.T) {
	rt := &stubRuntime{healthy: true, ready: true, card: &AgentCard{}}
	srv := newTestServer(rt)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}

	rt.healthy = false
	resp, _ = http.Get(srv.URL + "/healthz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unhealthy status = %d, want 503", resp.StatusCode)
	}
}

func TestReadyz(t *testing.T) {
	rt := &stubRuntime{healthy: true, ready: true, card: &AgentCard{}}
	srv := newTestServer(rt)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz status = %d, want 200", resp.StatusCode)
	}

	rt.ready = false
	resp, _ = http.Get(srv.URL + "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("not ready status = %d, want 503", resp.StatusCode)
	}
}

func TestMethodNotFound(t *testing.T) {
	rt := &stubRuntime{card: &AgentCard{}, healthy: true, ready: true}
	srv := newTestServer(rt)
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"unknown/method","params":{}}`
	resp, err := http.Post(srv.URL+"/", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error in response")
	}
	if errObj["code"].(float64) != -32601 {
		t.Errorf("error code = %v, want -32601", errObj["code"])
	}
}

// TestApplyHeaderFallbacks verifies the OGA-446 gateway-strip fallback: the
// X-Pending-Action-Context header is folded into message metadata when the body
// omitted it, and never overwrites a value the body already carried.
func TestApplyHeaderFallbacks(t *testing.T) {
	newMsg := func() *A2AMessage {
		return &A2AMessage{Params: &MessageParams{Message: &Message{Role: "user"}}}
	}

	// Header present, body absent → folded in.
	r1, _ := http.NewRequest(http.MethodPost, "/", nil)
	r1.Header.Set(HeaderPendingActionContext, `{"question":"which?"}`)
	m1 := newMsg()
	applyHeaderFallbacks(r1, m1)
	if got := m1.Params.Message.Metadata[MetadataKeyPendingActionContext]; got != `{"question":"which?"}` {
		t.Errorf("header not folded into metadata: %v", got)
	}

	// Body present → header must NOT overwrite.
	r2, _ := http.NewRequest(http.MethodPost, "/", nil)
	r2.Header.Set(HeaderPendingActionContext, "FROM-HEADER")
	m2 := newMsg()
	m2.Params.Message.Metadata = map[string]any{MetadataKeyPendingActionContext: "FROM-BODY"}
	applyHeaderFallbacks(r2, m2)
	if got := m2.Params.Message.Metadata[MetadataKeyPendingActionContext]; got != "FROM-BODY" {
		t.Errorf("body metadata must win, got %v", got)
	}

	// No header → no metadata created.
	r3, _ := http.NewRequest(http.MethodPost, "/", nil)
	m3 := newMsg()
	applyHeaderFallbacks(r3, m3)
	if m3.Params.Message.Metadata != nil {
		t.Errorf("no header should leave metadata nil, got %v", m3.Params.Message.Metadata)
	}
}
