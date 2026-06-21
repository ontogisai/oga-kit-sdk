package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

const (
	defaultShutdownTimeout   = 30 * time.Second
	defaultReadHeaderTimeout = 10 * time.Second
)

// ToolHandler runs a single tool: it receives the JSON-RPC arguments and
// returns a JSON-marshalable result, or an error. A returned error is surfaced
// to the caller as an MCP tools/call result with isError=true (the platform
// proxy carries it through unchanged) — it does NOT become a JSON-RPC
// transport error.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool is a registered tool: its MCP definition plus the handler that runs it.
type Tool struct {
	// Name is the unique tool name (e.g. "fm_get_building_overview").
	Name string
	// Description is the human-readable tool description surfaced in tools/list.
	Description string
	// InputSchema is the JSON Schema for the tool arguments (tools/list).
	InputSchema json.RawMessage
	// Handler runs the tool.
	Handler ToolHandler
}

// Runtime is the default MCP tool server runtime for domain-kit Tier 3
// sidecars. Construct it (NewRuntimeFromEnv or NewRuntime), register tools, and
// call ListenAndServe.
type Runtime struct {
	gw     *gateway.PlatformGatewayClient
	closer io.Closer

	mu       sync.RWMutex
	registry map[string]Tool
	order    []string // registration order, for deterministic tools/list

	ready bool
}

// NewRuntime builds a runtime around an already-constructed gateway client and
// an optional Closer (stopped on shutdown — typically the token-manager closer
// returned by gateway.NewClientFromEnv). Use this when you need to customise
// the client; most kits use NewRuntimeFromEnv. A nil closer is treated as a
// no-op.
func NewRuntime(gw *gateway.PlatformGatewayClient, closer io.Closer) *Runtime {
	if closer == nil {
		closer = noopCloser{}
	}
	return &Runtime{
		gw:       gw,
		closer:   closer,
		registry: make(map[string]Tool),
		ready:    true,
	}
}

// NewRuntimeFromEnv builds a runtime with an authenticated gateway client from
// the Sidecar-Manager-injected environment (OGA-404 bootstrap-mint via
// gateway.NewClientFromEnv). A bootstrap-mint failure is fatal and returned as
// an error — the sidecar must not serve without a verifiable identity.
func NewRuntimeFromEnv(ctx context.Context) (*Runtime, error) {
	gw, closer, err := gateway.NewClientFromEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway client from env: %w", err)
	}
	return NewRuntime(gw, closer), nil
}

// noopCloser is the zero-cost Closer used when none is supplied.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// Gateway returns the authenticated Platform Access Gateway client so tool
// handlers can call Tier 1/2 platform tools (kg_*) with a valid workload token.
func (r *Runtime) Gateway() *gateway.PlatformGatewayClient { return r.gw }

// Register adds a tool. It returns an error on an empty name, a nil handler, or
// a duplicate name.
func (r *Runtime) Register(tool Tool) error {
	if tool.Name == "" {
		return errors.New("tool name is required")
	}
	if tool.Handler == nil {
		return fmt.Errorf("tool %q has a nil handler", tool.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.registry[tool.Name]; dup {
		return fmt.Errorf("tool %q is already registered", tool.Name)
	}
	r.registry[tool.Name] = tool
	r.order = append(r.order, tool.Name)
	return nil
}

// RegisterFunc is a convenience wrapper over Register for the common case of
// registering a handler with its name, description, and schema inline.
func (r *Runtime) RegisterFunc(name, description string, schema json.RawMessage, handler ToolHandler) error {
	return r.Register(Tool{
		Name:        name,
		Description: description,
		InputSchema: schema,
		Handler:     handler,
	})
}

// ToolCount returns the number of registered tools.
func (r *Runtime) ToolCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.registry)
}

// Close stops the token manager (via the Closer supplied at construction).
// Safe to call multiple times.
func (r *Runtime) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// Handler builds the http.Handler that serves the MCP transport + health
// probes. Exposed so callers can mount it on a custom server or test it with
// httptest; ListenAndServe uses it internally.
func (r *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", r.ServeMCP)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		r.mu.RLock()
		ready := r.ready
		r.mu.RUnlock()
		if !ready {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return mux
}

// ListenAndServe starts the HTTP server on the given port and blocks until a
// SIGTERM/SIGINT signal or ctx cancellation, then performs a graceful shutdown
// and stops the token manager. This is the one-call entry point for a kit
// sidecar main().
func (r *Runtime) ListenAndServe(ctx context.Context, port string) {
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r.Handler(),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
	}

	go func() {
		slog.Info("mcptools runtime starting", "port", port, "tool_count", r.ToolCount())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("mcptools server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-stop:
		slog.Info("shutdown signal received", "signal", sig.String())
	case <-ctx.Done():
		slog.Info("context cancelled")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("mcptools shutdown failed", "error", err)
	}
	if err := r.Close(); err != nil {
		slog.Warn("mcptools token manager close failed", "error", err)
	}
	slog.Info("mcptools runtime shutdown complete")
}
