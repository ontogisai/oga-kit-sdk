package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ListenAndServe starts an HTTP server that serves the A2A protocol for any// AgentRuntime implementation. It handles graceful shutdown on SIGTERM/SIGINT.
func ListenAndServe(ctx context.Context, port string, runtime AgentRuntime) {
	mux := http.NewServeMux()

	// Agent card endpoints (both direct and via gateway prefix)
	cardHandler := agentCardHandler(runtime)
	mux.HandleFunc("GET /.well-known/agent-card.json", cardHandler)
	mux.HandleFunc("GET /agents/{name}/.well-known/agent-card.json", cardHandler)

	// Message endpoints
	messageHandler := messageHandlerFunc(runtime)
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
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors are still 200
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
