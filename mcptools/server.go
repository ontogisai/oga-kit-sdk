package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// Standard JSON-RPC 2.0 error codes (subset) used by the MCP transport.
const (
	jsonrpcErrParse         = -32700
	jsonrpcErrInvalidReq    = -32600
	jsonrpcErrMethodMissing = -32601
	jsonrpcErrInvalidParams = -32602

	maxRequestBytes = 1 << 20 // 1 MiB
)

// jsonrpcRequest is the inbound JSON-RPC 2.0 envelope (from the platform MCP
// Tool Server catalog proxy).
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// toolsCallParams is the params shape for the MCP tools/call method.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// contextKey is an unexported type for context keys to avoid collisions.
type contextKey string

const (
	ctxKeyTenantID    contextKey = "tenant_id"
	ctxKeyPrincipalID contextKey = "principal_id"
	ctxKeyRequestID   contextKey = "request_id"
)

// TenantFromContext returns the X-Tenant-ID the gateway forwarded with the
// tools/call, or "" when absent. Handlers use it for tenant-scoped derivation
// (e.g. deterministic IDs) and audit logging.
func TenantFromContext(ctx context.Context) string { return strFromCtx(ctx, ctxKeyTenantID) }

// PrincipalFromContext returns the X-Principal-ID the gateway forwarded, or "".
func PrincipalFromContext(ctx context.Context) string { return strFromCtx(ctx, ctxKeyPrincipalID) }

// RequestIDFromContext returns the X-Request-ID the gateway forwarded, or "".
func RequestIDFromContext(ctx context.Context) string { return strFromCtx(ctx, ctxKeyRequestID) }

func strFromCtx(ctx context.Context, key contextKey) string {
	if v, ok := ctx.Value(key).(string); ok {
		return v
	}
	return ""
}

// ServeMCP is the HTTP handler for the MCP JSON-RPC transport. It decodes the
// JSON-RPC envelope, injects the gateway-forwarded identity headers into the
// handler context, dispatches tools/list, tools/call, and ping, and writes the
// JSON-RPC response.
func (r *Runtime) ServeMCP(w http.ResponseWriter, req *http.Request) {
	defer func() { _ = req.Body.Close() }()

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxRequestBytes))
	if err != nil {
		writeJSONRPCError(w, nil, jsonrpcErrParse, "read body: "+err.Error())
		return
	}

	var rpc jsonrpcRequest
	if err := json.Unmarshal(body, &rpc); err != nil {
		writeJSONRPCError(w, nil, jsonrpcErrParse, "decode: "+err.Error())
		return
	}
	if rpc.JSONRPC != "2.0" {
		writeJSONRPCError(w, rpc.ID, jsonrpcErrInvalidReq, "jsonrpc must be 2.0")
		return
	}

	ctx := injectIdentity(req.Context(), req)

	switch rpc.Method {
	case "tools/list":
		writeJSONRPCResult(w, rpc.ID, r.listToolsResult())
	case "tools/call":
		r.handleToolsCall(ctx, w, rpc.ID, rpc.Params)
	case "ping":
		writeJSONRPCResult(w, rpc.ID, map[string]any{})
	default:
		writeJSONRPCError(w, rpc.ID, jsonrpcErrMethodMissing, "method not found: "+rpc.Method)
	}
}

// injectIdentity copies the gateway-forwarded identity headers into the
// request context so handlers can read them via TenantFromContext etc.
func injectIdentity(ctx context.Context, req *http.Request) context.Context {
	if t := req.Header.Get("X-Tenant-ID"); t != "" {
		ctx = context.WithValue(ctx, ctxKeyTenantID, t)
	}
	if p := req.Header.Get("X-Principal-ID"); p != "" {
		ctx = context.WithValue(ctx, ctxKeyPrincipalID, p)
	}
	if rid := req.Header.Get("X-Request-ID"); rid != "" {
		ctx = context.WithValue(ctx, ctxKeyRequestID, rid)
	}
	return ctx
}

// handleToolsCall dispatches a tools/call to the registered handler and wraps
// the result (or error) in the MCP content envelope.
func (r *Runtime) handleToolsCall(ctx context.Context, w http.ResponseWriter, id, rawParams json.RawMessage) {
	var params toolsCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		writeJSONRPCError(w, id, jsonrpcErrInvalidParams, "invalid tools/call params: "+err.Error())
		return
	}
	if params.Name == "" {
		writeJSONRPCError(w, id, jsonrpcErrInvalidParams, "tool name is required")
		return
	}

	r.mu.RLock()
	tool, ok := r.registry[params.Name]
	r.mu.RUnlock()
	if !ok {
		writeJSONRPCError(w, id, jsonrpcErrInvalidParams, "unknown tool: "+params.Name)
		return
	}

	slog.InfoContext(ctx, "tool dispatched",
		"tool_name", params.Name,
		"tenant_id", TenantFromContext(ctx),
		"request_id", RequestIDFromContext(ctx),
	)

	result, err := tool.Handler(ctx, params.Arguments)
	if err != nil {
		// MCP convention: tool errors are results with isError=true, not
		// JSON-RPC transport errors. The platform proxy carries them through.
		writeJSONRPCResult(w, id, mcpErrorResult(err))
		return
	}
	writeJSONRPCResult(w, id, mcpSuccessResult(result))
}

// listToolsResult builds the tools/list response from the registered tools, in
// registration order for deterministic output.
func (r *Runtime) listToolsResult() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		t := r.registry[name]
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return map[string]any{"tools": out}
}

// mcpSuccessResult wraps a tool result in the MCP tools/call content envelope.
func mcpSuccessResult(result any) map[string]any {
	body, err := json.Marshal(result)
	if err != nil {
		return mcpErrorResult(fmt.Errorf("marshal result: %w", err))
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(body)},
		},
	}
}

// mcpErrorResult wraps a tool error in the MCP content envelope with isError=true.
func mcpErrorResult(err error) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": err.Error()},
		},
		"isError": true,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("mcptools: encode response failed", "error", err)
	}
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, http.StatusOK, jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: msg},
	})
}
