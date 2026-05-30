package loader

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Handler returns an [http.Handler] that exposes the loader contract for the
// supplied [LoaderHandler]. Kit authors call this when they want to compose
// the loader endpoints with their own middleware (auth, logging, metrics)
// before handing the result to [http.Server].
//
// For the simple case where no extra middleware is needed, use
// [ListenAndServe] which wraps Handler with health checks and graceful
// shutdown.
func Handler(impl LoaderHandler) http.Handler {
	if impl == nil {
		panic("loader.Handler: impl is nil")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /load", loadHandlerFunc(impl))
	mux.HandleFunc("GET /jobs/{id}", jobHandlerFunc(impl))
	mux.HandleFunc("GET /formats", formatsHandlerFunc(impl))
	mux.HandleFunc("GET /healthz", healthHandlerFunc(impl))
	return mux
}

// ServerConfig tunes the [ListenAndServe] HTTP server. Zero values pick
// reasonable defaults documented per field.
type ServerConfig struct {
	// Port is the TCP port to listen on (without colon). Required.
	Port string

	// ReadHeaderTimeout caps how long the loader will wait for headers
	// before resetting the connection. Default 10s.
	ReadHeaderTimeout time.Duration

	// ReadTimeout caps the total time to read the request body.
	// Default 60s. Increase for loaders that accept very large request
	// bodies inline (rare — most loaders stream from SourceURI).
	ReadTimeout time.Duration

	// WriteTimeout caps how long a synchronous handler may take before
	// the loader resets the connection. Default 5 min — synchronous
	// loaders for moderate datasets sit comfortably inside this. Async
	// loaders return immediately so this only affects /load on the
	// synchronous path.
	WriteTimeout time.Duration

	// IdleTimeout caps how long an idle keepalive connection stays
	// open. Default 60s.
	IdleTimeout time.Duration

	// ShutdownTimeout caps the graceful shutdown phase. Default 30s.
	ShutdownTimeout time.Duration

	// Logger is used for startup / shutdown messages. Defaults to
	// slog.Default().
	Logger *slog.Logger
}

func (c *ServerConfig) defaults() {
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 60 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 5 * time.Minute
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// ListenAndServe starts an HTTP server that exposes [Handler] for impl on
// the given port. It blocks until ctx is cancelled or a SIGTERM/SIGINT
// signal arrives, then runs a graceful shutdown.
//
// The simple call site for kit authors is:
//
//	if err := loader.ListenAndServe(ctx, &loader.ServerConfig{Port: "8400"}, myImpl); err != nil {
//	    slog.Error("loader serve failed", "error", err)
//	    os.Exit(1)
//	}
func ListenAndServe(ctx context.Context, cfg *ServerConfig, impl LoaderHandler) error {
	if cfg == nil {
		return errors.New("loader.ListenAndServe: nil config")
	}
	if cfg.Port == "" {
		return errors.New("loader.ListenAndServe: port is required")
	}
	cfg.defaults()

	server := &http.Server{
		Addr:              ":" + strings.TrimPrefix(cfg.Port, ":"),
		Handler:           Handler(impl),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// Run the server in a goroutine so the main flow can wait on signals
	// or context cancellation.
	serveErr := make(chan error, 1)
	go func() {
		cfg.Logger.Info("loader sidecar listening", "port", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		cfg.Logger.Info("loader sidecar shutdown signal", "signal", sig.String())
	case <-ctx.Done():
		cfg.Logger.Info("loader sidecar context cancelled")
	case err := <-serveErr:
		// Server exited on its own (probably an error during start).
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		cfg.Logger.Error("loader sidecar shutdown error", "error", err)
		return err
	}
	cfg.Logger.Info("loader sidecar shutdown complete")
	return nil
}

// --- HTTP handler funcs ---

func loadHandlerFunc(impl LoaderHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LoadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", "decode request: "+err.Error())
			return
		}
		if req.TenantID == "" {
			writeError(w, http.StatusBadRequest, "missing_tenant_id", "tenant_id is required")
			return
		}
		if req.SourceURI == "" {
			writeError(w, http.StatusBadRequest, "missing_source_uri", "source_uri is required")
			return
		}

		resp, err := impl.Load(r.Context(), &req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load_failed", err.Error())
			return
		}
		if resp == nil {
			writeError(w, http.StatusInternalServerError, "nil_response", "loader returned nil response")
			return
		}

		// 202 for non-terminal (async) status; 200 for terminal (sync) status.
		status := http.StatusOK
		if !resp.Status.IsTerminal() {
			status = http.StatusAccepted
		}
		writeJSON(w, status, resp)
	}
}

func jobHandlerFunc(impl LoaderHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID := r.PathValue("id")
		if jobID == "" {
			writeError(w, http.StatusBadRequest, "missing_job_id", "job id is required in path")
			return
		}

		resp, err := impl.Job(r.Context(), jobID)
		if err != nil {
			if IsJobNotFound(err) {
				writeError(w, http.StatusNotFound, "job_not_found", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "job_lookup_failed", err.Error())
			return
		}
		if resp == nil {
			writeError(w, http.StatusInternalServerError, "nil_response", "loader returned nil response")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func formatsHandlerFunc(impl LoaderHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		formats, err := impl.Formats(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "formats_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, &FormatsResponse{Formats: formats})
	}
}

func healthHandlerFunc(impl LoaderHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := impl.Health(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, &HealthResponse{
				Status:  "unhealthy",
				Message: err.Error(),
			})
			return
		}
		if resp == nil {
			resp = &HealthResponse{Status: "ok"}
		}
		statusCode := http.StatusOK
		if resp.Status != "ok" {
			statusCode = http.StatusServiceUnavailable
		}
		writeJSON(w, statusCode, resp)
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers already written; best we can do is log via slog.
		slog.Error("loader: write response", "error", err, "status", status)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, &ErrorResponse{Code: code, Message: message})
}
