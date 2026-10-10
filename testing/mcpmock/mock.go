// Package mcpmock provides an in-process mock MCP server for testing domain
// agents without a running platform.
package mcpmock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// MockMCPServer is a test MCP server that records tool calls and returns
// configured responses.
type MockMCPServer struct {
	t      testing.TB
	server *httptest.Server
	mu     sync.Mutex
	tools  map[string]ToolHandler
	calls  map[string]int
}

// ToolHandler is a function that handles a tool call and returns a result.
type ToolHandler func(params json.RawMessage) (any, error)

// NewServer creates a new mock MCP server for testing.
func NewServer(t testing.TB) *MockMCPServer {
	t.Helper()

	m := &MockMCPServer{
		t:     t,
		tools: make(map[string]ToolHandler),
		calls: make(map[string]int),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp/tools/call", m.handleToolCall)

	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)

	return m
}

// RegisterTool registers a tool handler.
func (m *MockMCPServer) RegisterTool(name string, handler ToolHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools[name] = handler
}

// URL returns the mock server's URL.
func (m *MockMCPServer) URL() string {
	return m.server.URL
}

// CallCount returns the number of times a tool was called.
func (m *MockMCPServer) CallCount(toolName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[toolName]
}

// Reset clears all call counts.
func (m *MockMCPServer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = make(map[string]int)
}

func (m *MockMCPServer) handleToolCall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool   string          `json:"tool"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.calls[req.Tool]++
	handler, ok := m.tools[req.Tool]
	m.mu.Unlock()

	if !ok {
		http.Error(w, "tool not found: "+req.Tool, http.StatusNotFound)
		return
	}

	result, err := handler(req.Params)
	if err != nil {
		resp := map[string]any{"error": err.Error()}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
