package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// PlatformGatewayClient provides access to all platform services through the
// Platform Access Gateway. It handles authentication, token refresh, and
// request routing.
type PlatformGatewayClient struct {
	baseURL   string
	tokenPath string
	tenantID  string
	client    *http.Client

	mu    sync.RWMutex
	token string

	// tokenProvider, when set, is the authoritative source of the current
	// bearer token. It is wired to a TokenManager that performs sliding
	// renewal against /auth/token/refresh, so the client always sends a
	// fresh token. When nil, the client falls back to reading tokenPath.
	tokenProvider func() string
}

// NewPlatformGatewayClient creates a new gateway client.
func NewPlatformGatewayClient(baseURL, tokenPath, tenantID string) *PlatformGatewayClient {
	return &PlatformGatewayClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		tenantID:  tenantID,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// SetTokenProvider wires an authoritative token source (typically a
// *auth.TokenManager's Token method). Once set, every request reads the bearer
// from the provider instead of the cached file value, so rotated tokens are
// picked up immediately.
func (c *PlatformGatewayClient) SetTokenProvider(provider func() string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenProvider = provider
}

// CallTool invokes an MCP tool via the Platform Access Gateway.
// Returns a ToolError (which implements error) when the tool returns a structured
// error response, allowing callers to inspect the error code, category, and details.
func (c *PlatformGatewayClient) CallTool(ctx context.Context, tool string, params any) (json.RawMessage, error) {
	body := map[string]any{
		"tool":   tool,
		"params": params,
	}
	resp, err := c.post(ctx, "/mcp/tools/call", body)
	if err != nil {
		// Try to parse as a structured tool error from the gateway
		if toolErr := ParseToolError(err); toolErr != nil {
			toolErr.ToolName = tool
			return nil, toolErr
		}
		return nil, fmt.Errorf("call tool %s: %w", tool, err)
	}
	return resp, nil
}

// ToolSchema describes a single MCP tool discovered via the Platform Access
// Gateway's tool-discovery endpoint (OGA-431). InputSchema is the tool's JSON
// Schema carried verbatim so the ReAct planner can render an argument summary
// (names + types, required marked) and the LLM emits correct arguments instead
// of guessing parameter names. Kept in the gateway package (not agent) so the
// gateway client has no import dependency on agent — streampipeline maps this
// to agent.ToolSchema.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Mutates marks a state-changing tool (OGA-446). Carried from the platform
	// tools/list so the ReAct planner's confirm-before-write gate sees the
	// authoritative flag instead of relying on a tool-name heuristic. Pointer so
	// "absent" (nil → heuristic) is distinct from an explicit false (read).
	Mutates *bool `json:"mutates,omitempty"`
}

// toolsListResponse is the body returned by GET-style POST /mcp/tools/list.
type toolsListResponse struct {
	Tools []ToolSchema `json:"tools"`
}

// ListTools discovers the MCP tools the authenticated agent is allowed to call,
// returning each tool's name, description, and input schema (OGA-431). The
// gateway filters the result to the agent's tool allowlist, so the returned set
// is exactly what the agent may attempt. Used by the SDK ReAct planner to
// render correct tool-argument summaries.
func (c *PlatformGatewayClient) ListTools(ctx context.Context) ([]ToolSchema, error) {
	resp, err := c.post(ctx, "/mcp/tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	var out toolsListResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("parse tools/list response: %w", err)
	}
	return out.Tools, nil
}

// ToolError represents a structured error returned by an MCP tool.
// It provides programmatic access to the error code, category, and details
// so callers can handle specific error types (e.g., distinguish "entity type
// not in ontology" from "PBAC denied" from "internal error").
type ToolError struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Category   string         `json:"category"`
	Service    string         `json:"service"`
	Details    map[string]any `json:"details,omitempty"`
	DocURL     string         `json:"doc_url,omitempty"`
	Retry      bool           `json:"retry"`
	HTTPStatus int            `json:"http_status"`
	ToolName   string         `json:"-"`
}

// Error implements the error interface.
func (e *ToolError) Error() string {
	if e.ToolName != "" {
		return fmt.Sprintf("tool %s: [%s] %s", e.ToolName, e.Code, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// IsValidationError returns true if this is a schema/input validation error.
func (e *ToolError) IsValidationError() bool {
	return e.Category == "VAL"
}

// IsPermissionError returns true if this is a PBAC/access denial.
func (e *ToolError) IsPermissionError() bool {
	return e.Category == "DENY"
}

// IsNotFound returns true if the requested resource was not found.
func (e *ToolError) IsNotFound() bool {
	return e.Category == "NFND"
}

// ParseToolError attempts to extract a structured ToolError from a gateway
// error response. Returns nil if the error is not a structured tool error.
func ParseToolError(err error) *ToolError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// Gateway errors come as: "gateway error {status}: {json_body}"
	idx := indexOf(msg, ": {")
	if idx < 0 {
		return nil
	}
	jsonPart := msg[idx+2:]
	var toolErr ToolError
	if json.Unmarshal([]byte(jsonPart), &toolErr) == nil && toolErr.Code != "" {
		return &toolErr
	}
	return nil
}

// indexOf returns the index of substr in s, or -1.
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// ChatCompletionRequest is the request for LLM chat completion.
type ChatCompletionRequest struct {
	Messages  []ChatMessage `json:"messages"`
	Model     string        `json:"model,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream,omitempty"`
	RequestID string        `json:"-"`

	// Temperature overrides the sampling temperature. Pointer so 0.0 (fully
	// deterministic) is distinguishable from "unset" (use the gateway default).
	Temperature *float64 `json:"temperature,omitempty"`

	// StreamOptions tunes streaming behavior. Set IncludeUsage to ask the proxy
	// to emit a final usage-bearing chunk (OGA-420) so streaming responses
	// surface token counts instead of dropping them. Ignored when Stream is false.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions tunes a streaming chat completion.
type StreamOptions struct {
	// IncludeUsage requests a final chunk carrying token usage (OpenAI-compatible
	// stream_options.include_usage). The proxy emits it after the content chunks
	// with empty Choices and a populated Usage.
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage is a single message in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionResponse is the response from LLM chat completion.
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage,omitempty"`
}

// ChatChoice is a single choice in a chat completion response.
type ChatChoice struct {
	Index   int         `json:"index"`
	Message ChatMessage `json:"message"`
}

// Usage reports token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletion performs a synchronous LLM chat completion via the gateway.
func (c *PlatformGatewayClient) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	body := map[string]any{
		"messages": req.Messages,
		"stream":   false,
	}
	if req.Model != "" {
		body["model"] = req.Model
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	respData, err := c.post(ctx, "/llm/v1/chat/completions", body)
	if err != nil {
		return nil, fmt.Errorf("chat completion: %w", err)
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}
	return &resp, nil
}

// ChatCompletionStream performs a streaming LLM chat completion.
// Returns a channel that receives chunks as they arrive.
func (c *PlatformGatewayClient) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (<-chan *ChatChunk, error) {
	body := map[string]any{
		"messages": req.Messages,
		"stream":   true,
	}
	if req.Model != "" {
		body["model"] = req.Model
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.StreamOptions != nil {
		body["stream_options"] = req.StreamOptions
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/llm/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("streaming request failed: %s", resp.Status)
	}

	ch := make(chan *ChatChunk, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)
		dec := json.NewDecoder(resp.Body)
		for {
			var chunk ChatChunk
			if err := dec.Decode(&chunk); err != nil {
				return
			}
			select {
			case ch <- &chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// ChatChunk is a single chunk in a streaming chat completion.
type ChatChunk struct {
	ID      string            `json:"id"`
	Choices []ChatChunkChoice `json:"choices"`

	// Usage is populated only on the final chunk when the request set
	// StreamOptions.IncludeUsage (OGA-420). Nil on content chunks.
	Usage *Usage `json:"usage,omitempty"`
}

// ChatChunkChoice is a choice within a streaming chunk.
type ChatChunkChoice struct {
	Index int       `json:"index"`
	Delta ChatDelta `json:"delta"`
}

// ChatDelta is the incremental content in a streaming chunk.
type ChatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// SubmitWorkflow submits a workflow for execution (e.g., HITL approval).
func (c *PlatformGatewayClient) SubmitWorkflow(ctx context.Context, workflowType string, input any) (string, error) {
	body := map[string]any{
		"type":  workflowType,
		"input": input,
	}
	respData, err := c.post(ctx, "/workflow", body)
	if err != nil {
		return "", fmt.Errorf("submit workflow: %w", err)
	}

	var result struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return "", fmt.Errorf("parse workflow response: %w", err)
	}
	return result.WorkflowID, nil
}

// AgentCard is the A2A agent card returned by GetAgentCard.
type AgentCard struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Version     string `json:"version"`
}

// GetAgentCard retrieves another agent's A2A card via the gateway.
func (c *PlatformGatewayClient) GetAgentCard(ctx context.Context, agentName string) (*AgentCard, error) {
	path := fmt.Sprintf("/agents/%s/.well-known/agent-card.json", agentName)
	respData, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get agent card %s: %w", agentName, err)
	}

	var card AgentCard
	if err := json.Unmarshal(respData, &card); err != nil {
		return nil, fmt.Errorf("parse agent card: %w", err)
	}
	return &card, nil
}

// A2AInvokeRequest is the request for invoking another agent.
type A2AInvokeRequest struct {
	Method  string          `json:"method"`
	Message json.RawMessage `json:"message"`
}

// InvokeAgent sends a message/send request to another agent via the gateway.
func (c *PlatformGatewayClient) InvokeAgent(ctx context.Context, agentName string, msg any) (json.RawMessage, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "message/send",
		"params":  map[string]any{"message": msg},
	}
	path := fmt.Sprintf("/agents/%s", agentName)
	resp, err := c.post(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("invoke agent %s: %w", agentName, err)
	}
	return resp, nil
}

// InvokeAgentStream sends a message/stream request to another agent and yields
// the sub-agent's stream-event JSON payloads. The downstream A2A endpoint emits
// Server-Sent Events (`event: <type>\ndata: <JSON StreamEvent>\n\n`), so this
// parses the SSE framing and emits each event's `data:` JSON as a
// *json.RawMessage. SSE comments, `event:` lines, blank separators, and the
// `[DONE]` sentinel are skipped. Callers (the streampipeline delegation path)
// decode each raw message into an agent.StreamEvent and re-parent it.
func (c *PlatformGatewayClient) InvokeAgentStream(ctx context.Context, agentName string, msg any) (<-chan *json.RawMessage, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "message/stream",
		"params":  map[string]any{"message": msg},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/agents/%s", agentName)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("agent stream request failed: %s", resp.Status)
	}

	ch := make(chan *json.RawMessage, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		// SSE data payloads can be large (a tool_result preview); raise the
		// scanner's line cap well above the 64KB default.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		dataLines := make([]string, 0, 4)
		flush := func() {
			if len(dataLines) == 0 {
				return
			}
			data := strings.TrimSpace(strings.Join(dataLines, "\n"))
			dataLines = dataLines[:0]
			if data == "" || data == "[DONE]" {
				return
			}
			raw := json.RawMessage(data)
			select {
			case ch <- &raw:
			case <-ctx.Done():
			}
		}

		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, "data:"):
				dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
			default:
				// `event:` lines, `id:` lines, and `:` comments are ignored —
				// the StreamEvent JSON in `data:` already carries its type.
			}
		}
		flush() // emit a trailing event with no terminating blank line
	}()

	return ch, nil
}

// RegistrationRequest is the request for self-registration.
type RegistrationRequest struct {
	AgentID      string   `json:"agent_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	URL          string   `json:"url"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// RegisterSelf registers this agent in the Agent Registry.
func (c *PlatformGatewayClient) RegisterSelf(ctx context.Context, reg *RegistrationRequest) error {
	_, err := c.post(ctx, "/registry/register", reg)
	if err != nil {
		return fmt.Errorf("register self: %w", err)
	}
	return nil
}

// DeregisterSelf removes this agent from the Agent Registry.
func (c *PlatformGatewayClient) DeregisterSelf(ctx context.Context) error {
	_, err := c.post(ctx, "/registry/deregister", nil)
	if err != nil {
		return fmt.Errorf("deregister self: %w", err)
	}
	return nil
}

// post sends a POST request to the gateway and returns the response body.
func (c *PlatformGatewayClient) post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gateway error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// get sends a GET request to the gateway and returns the response body.
func (c *PlatformGatewayClient) get(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gateway error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func (c *PlatformGatewayClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", c.tenantID)

	token := c.loadToken()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (c *PlatformGatewayClient) loadToken() string {
	c.mu.RLock()
	provider := c.tokenProvider
	cached := c.token
	c.mu.RUnlock()

	// Authoritative source when wired (TokenManager with sliding renewal).
	if provider != nil {
		if tok := provider(); tok != "" {
			return tok
		}
	}

	if cached != "" {
		return cached
	}

	if c.tokenPath == "" {
		return ""
	}

	data, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return ""
	}

	token := strings.TrimSpace(string(data))

	c.mu.Lock()
	c.token = token
	c.mu.Unlock()

	return token
}

// InvalidateToken clears the cached token, forcing a reload on next request.
func (c *PlatformGatewayClient) InvalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}
