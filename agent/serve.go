package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/ontogisai/oga-kit-sdk/sidecarenv"
)

// ListenAndServe starts an HTTP server that serves the A2A protocol for any
// AgentRuntime implementation. It handles graceful shutdown on SIGTERM/SIGINT.
// It answers a message only for the agent's own tenant, sidecarenv.TenantID
// (requireAgentTenant).
func ListenAndServe(ctx context.Context, port string, runtime AgentRuntime) {
	tenantID := os.Getenv(sidecarenv.TenantID)
	if tenantID == "" {
		slog.Warn("agent has no tenant: every A2A message will be refused", "unset_env", sidecarenv.TenantID)
	}
	mux := newAgentMux(runtime, tenantID)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("agent listening", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
		slog.Info("context cancelled")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	slog.Info("agent shutdown complete")
}

// newAgentMux builds the agent's routes. Both message routes accept only a
// request for the agent's own tenant (requireAgentTenant); the card and the
// probes are open.
func newAgentMux(runtime AgentRuntime, tenantID string) *http.ServeMux {
	mux := http.NewServeMux()

	// Agent card endpoints (both direct and via gateway prefix)
	cardHandler := agentCardHandler(runtime)
	mux.HandleFunc("GET /.well-known/agent-card.json", cardHandler)
	mux.HandleFunc("GET /agents/{name}/.well-known/agent-card.json", cardHandler)

	// Message endpoints
	messageHandler := requireAgentTenant(tenantID, messageHandlerFunc(runtime))
	mux.HandleFunc("POST /", messageHandler)
	mux.HandleFunc("POST /agents/{name}", messageHandler)

	// Health probes
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := runtime.Healthz(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := runtime.Readyz(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// HeaderTenantID carries the caller's tenant. Every platform caller of an agent
// sets it: agentgateway from the verified token, Frontier and the event router
// for the tenant they dispatch for, the Platform Gateway from the calling
// sidecar's workload token.
const HeaderTenantID = "X-Tenant-ID"

// jsonRPCErrTenant is the JSON-RPC error code of a message for another tenant
// (the -32000..-32099 range is for server-defined errors).
const jsonRPCErrTenant = -32001

// requireAgentTenant refuses (403) a message whose X-Tenant-ID is absent, holds
// more than one value, or is not the agent's tenant. A kit agent serves one
// tenant and acts with that tenant's workload token, so a message for any
// other tenant would be answered from the wrong tenant's data. The gateway
// route already admits only the agent's tenant (OGA-1259); this is the second
// check, for any path that reaches the agent without that route. An agent
// without a tenant refuses every message.
func requireAgentTenant(tenantID string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tenantID != "" && singleHeaderValue(r.Header, HeaderTenantID) == tenantID {
			next(w, r)
			return
		}
		// The header's value is the caller's, so it is not logged.
		slog.WarnContext(r.Context(), "a2a message refused: X-Tenant-ID is not this agent's tenant",
			"has_tenant_header", len(r.Header.Values(HeaderTenantID)) > 0)
		writeJSONRPCErrorStatus(w, nil, http.StatusForbidden, jsonRPCErrTenant,
			"the request's tenant (X-Tenant-ID) is not this agent's tenant")
	}
}

// singleHeaderValue returns the header's value when every non-blank occurrence
// holds the same one, and "" otherwise.
func singleHeaderValue(h http.Header, name string) string {
	value := ""
	for _, v := range h.Values(name) {
		v = strings.TrimSpace(v)
		switch {
		case v == "":
		case value == "":
			value = v
		case v != value:
			return ""
		}
	}
	return value
}

func agentCardHandler(runtime AgentRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		card := runtime.ServeAgentCard()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}
}

func messageHandlerFunc(runtime AgentRuntime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg A2AMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error())
			return
		}

		// Header fallbacks (OGA-446): an A2A gateway MAY strip params.metadata
		// on proxy (OGA-276). When the resume token is absent from the body but
		// present as a header, fold it into the message metadata so the reactive
		// resume survives a stripping proxy. No-op when the body already carries it.
		applyHeaderFallbacks(r, &msg)

		switch msg.Method {
		case "message/send":
			resp, err := runtime.HandleMessage(r.Context(), &msg)
			if err != nil {
				writeJSONRPCError(w, msg.ID, -32000, err.Error())
				return
			}
			writeJSONRPCResult(w, msg.ID, resp)

		case "message/stream":
			// Set SSE headers
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			flusher, ok := w.(http.Flusher)
			if !ok {
				writeJSONRPCError(w, msg.ID, -32000, "streaming not supported")
				return
			}

			stream := &httpStreamWriter{w: w, flusher: flusher}
			if err := runtime.HandleStream(r.Context(), &msg, stream); err != nil {
				slog.Error("stream error", "error", err)
			}

		default:
			writeJSONRPCError(w, msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method))
		}
	}
}

// httpStreamWriter serializes typed StreamEvents to SSE wire format. The
// event-type SSE field carries the EventType string ("task/reasoning" etc.);
// the data SSE field carries the entire JSON-marshalled StreamEvent envelope
// so consumers (the platform's Frontier HTTPStreamingInvoker) can deserialize
// directly into a typed *StreamEvent without lossy coercion.
type httpStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *httpStreamWriter) WriteEvent(_ context.Context, event *StreamEvent) error {
	if event == nil {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event.Type, data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Close emits a final task/status{completed} event so consumers see a clean
// terminal marker. Pipelines that emit their own terminal status before Close
// will produce a duplicate completed status — that's harmless (consumers
// process events idempotently by sequence number).
func (s *httpStreamWriter) Close() error {
	final := &StreamEvent{
		TaskID:    uuid.New().String(),
		Sequence:  -1, // sentinel for terminal close (consumers should ignore if they tracked their own seq)
		Timestamp: time.Now().UTC(),
		Type:      EventTypeStatus,
		Payload:   &StatusPayload{State: TaskStateCompleted},
	}
	data, err := json.Marshal(final)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", final.Type, data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSONRPCErrorStatus(w, id, http.StatusOK, code, message) // JSON-RPC errors are still 200
}

// writeJSONRPCErrorStatus writes a JSON-RPC error with the given HTTP status,
// for a refusal the transport should see as one.
func writeJSONRPCErrorStatus(w http.ResponseWriter, id json.RawMessage, status, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// A2A metadata keys + their gateway-strip header fallbacks (OGA-446). The
// metadata key is the contract the reactive handler reads
// (streampipeline references MetadataKeyPendingActionContext); the header is set
// by a caller (the platform Frontier) alongside params.metadata so a proxy that
// strips the body metadata (OGA-276) does not silently break reactive resume.
const (
	MetadataKeyPendingActionContext = "pending_action_context"
	HeaderPendingActionContext      = "X-Pending-Action-Context"
)

// applyHeaderFallbacks folds header-carried context into the inbound message
// metadata when the body did not carry it. Body metadata always wins (a present
// key is never overwritten). Currently handles the OGA-446 resume token.
func applyHeaderFallbacks(r *http.Request, msg *A2AMessage) {
	if r == nil || msg == nil || msg.Params == nil || msg.Params.Message == nil {
		return
	}
	v := r.Header.Get(HeaderPendingActionContext)
	if v == "" {
		return
	}
	m := msg.Params.Message
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	if _, present := m.Metadata[MetadataKeyPendingActionContext]; !present {
		m.Metadata[MetadataKeyPendingActionContext] = v
	}
}
