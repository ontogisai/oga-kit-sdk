package loader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Handler returns an [http.Handler] that exposes the loader contract
// for the supplied [LoaderHandler]. Kit authors call this when they
// want to compose the loader endpoints with their own middleware
// (auth, logging, metrics) before handing the result to [http.Server].
//
// For the simple case where no extra middleware is needed, use
// [ListenAndServe] which wraps Handler with health checks and
// graceful shutdown.
//
// When impl additionally implements [StreamingLoaderHandler], the
// /load route drives the Plan / Pass loop instead of calling Load.
// The transition is invisible to the kit author beyond declaring the
// streaming methods on their type.
//
// The transfer.Writer used during /load is constructed via the
// supplied [HandlerOptions]. When opts.WriterFactory is nil the
// handler runs in "no-writer" mode and the kit handler receives a
// nil Transfer — useful for tests, but production deployments always
// supply a factory pointing at the platform gateway.
func Handler(impl LoaderHandler, opts ...HandlerOption) http.Handler {
	if impl == nil {
		panic("loader.Handler: impl is nil")
	}
	cfg := newHandlerConfig(opts)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /load", loadHandlerFunc(impl, cfg))
	mux.HandleFunc("GET /jobs/{id}", jobHandlerFunc(impl))
	mux.HandleFunc("GET /formats", formatsHandlerFunc(impl))
	mux.HandleFunc("GET /healthz", healthHandlerFunc(impl))
	return mux
}

// HandlerOption tunes a [Handler] at construction time.
type HandlerOption func(*handlerConfig)

// WriterFactory builds a [transfer.Writer] for a given LoadRequest.
// The SDK's HTTP server calls this once per /load invocation, hands
// the resulting writer to the kit handler via [LoadContext.Transfer],
// and closes the writer for the kit when Load (or the final Pass)
// returns nil. The factory receives the per-request kind (ontology
// vs data) determined by the kit's manifest declaration; it does not
// have to inspect the request body.
type WriterFactory func(ctx context.Context, kind transfer.LoadKind, req *LoadRequest) (transfer.Writer, error)

// WithWriterFactory sets the writer factory. Required for any deployment
// that actually persists records — the default factory returns an
// error explaining the misconfiguration.
func WithWriterFactory(f WriterFactory) HandlerOption {
	return func(c *handlerConfig) {
		if f != nil {
			c.writerFactory = f
		}
	}
}

// WithLoaderKind tells the server which transfer.LoadKind the writer
// factory should produce. Kits set this once at startup based on
// their manifest declaration — the brick ontology loader passes
// transfer.KindOntology, the brick data loader passes
// transfer.KindData. The server passes the kind through to the
// factory; kits never have to plumb it through their handler.
func WithLoaderKind(kind transfer.LoadKind) HandlerOption {
	return func(c *handlerConfig) {
		c.kind = kind
	}
}

type handlerConfig struct {
	writerFactory WriterFactory
	kind          transfer.LoadKind
}

func newHandlerConfig(opts []HandlerOption) *handlerConfig {
	c := &handlerConfig{
		// Default factory returns an error so misconfigured deployments
		// fail loudly instead of silently dropping records.
		writerFactory: func(_ context.Context, _ transfer.LoadKind, _ *LoadRequest) (transfer.Writer, error) {
			return nil, errors.New("loader.Handler: no transfer writer factory configured (use WithWriterFactory)")
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ServerConfig tunes the [ListenAndServe] HTTP server. Zero values
// pick reasonable defaults documented per field.
type ServerConfig struct {
	// Port is the TCP port to listen on (without colon). Required.
	Port string

	// ReadHeaderTimeout caps how long the loader will wait for headers
	// before resetting the connection. Default 10s.
	ReadHeaderTimeout time.Duration

	// ReadTimeout caps the total time to read the request body.
	// Default 60s.
	ReadTimeout time.Duration

	// WriteTimeout caps how long a synchronous handler may take.
	// Default 5 min.
	WriteTimeout time.Duration

	// IdleTimeout caps how long an idle keepalive connection stays
	// open. Default 60s.
	IdleTimeout time.Duration

	// ShutdownTimeout caps the graceful shutdown phase. Default 30s.
	ShutdownTimeout time.Duration

	// Logger is used for startup / shutdown messages. Defaults to
	// slog.Default().
	Logger *slog.Logger

	// HandlerOptions are passed to [Handler] when constructing the
	// underlying mux. Use this to set the writer factory and loader
	// kind in production.
	HandlerOptions []HandlerOption
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

// ListenAndServe starts an HTTP server that exposes [Handler] for
// impl on the given port. It blocks until ctx is cancelled or a
// SIGTERM/SIGINT signal arrives, then runs a graceful shutdown.
//
// Production usage:
//
//	commitClient, _ := transfer.NewHTTPCommitClient(gatewayURL, tenantID, kitID)
//	factory := func(ctx context.Context, kind transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
//	    return transfer.NewWriter(commitClient, kind, kitID), nil
//	}
//	cfg := &loader.ServerConfig{
//	    Port: "8400",
//	    HandlerOptions: []loader.HandlerOption{
//	        loader.WithWriterFactory(factory),
//	        loader.WithLoaderKind(transfer.KindData),
//	    },
//	}
//	loader.ListenAndServe(ctx, cfg, myImpl)
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
		Handler:           Handler(impl, cfg.HandlerOptions...),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

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

func loadHandlerFunc(impl LoaderHandler, cfg *handlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeAndValidateLoadRequest(w, r)
		if !ok {
			return
		}

		writer, err := cfg.writerFactory(r.Context(), cfg.kind, req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "writer_factory_failed", err.Error())
			return
		}
		lc := &LoadContext{
			Request:  req,
			Transfer: writer,
		}

		// Detect streaming variant. When implemented, drive the
		// Plan / Pass loop and skip the single-pass Load entirely.
		if streamer, isStreaming := impl.(StreamingLoaderHandler); isStreaming {
			runStreamingLoad(w, r, streamer, lc, writer)
			return
		}

		runSinglePassLoad(w, r, impl, lc, writer)
	}
}

// decodeAndValidateLoadRequest parses the JSON body, applies the
// authoritative tenant from the X-Tenant-ID header, and validates
// required fields. Writes the error response and returns ok=false on
// any failure so the caller can stop early.
func decodeAndValidateLoadRequest(w http.ResponseWriter, r *http.Request) (*LoadRequest, bool) {
	var req LoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "decode request: "+err.Error())
		return nil, false
	}

	// Tenant boundary: X-Tenant-ID is authoritative. The body's
	// tenant_id (if any) is overwritten — never trusted. This
	// matches the gateway-side behavior so kits and platform
	// agree on the tenancy identity.
	headerTenant := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if headerTenant == "" {
		writeError(w, http.StatusBadRequest, "missing_tenant_header",
			"X-Tenant-ID header is required (set by the gateway)")
		return nil, false
	}
	if req.TenantID != "" && req.TenantID != headerTenant {
		writeError(w, http.StatusBadRequest, "tenant_mismatch",
			fmt.Sprintf("body tenant_id %q does not match X-Tenant-ID header %q",
				req.TenantID, headerTenant))
		return nil, false
	}
	req.TenantID = headerTenant

	if req.SourceURI == "" {
		writeError(w, http.StatusBadRequest, "missing_source_uri", "source_uri is required")
		return nil, false
	}
	return &req, true
}

// runSinglePassLoad drives a single-pass [LoaderHandler]. Closes the
// writer once on success.
func runSinglePassLoad(w http.ResponseWriter, r *http.Request, impl LoaderHandler, lc *LoadContext, writer transfer.Writer) {
	ctx := r.Context()
	resp, err := impl.Load(ctx, lc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load_failed", err.Error())
		return
	}
	if resp == nil {
		writeError(w, http.StatusInternalServerError, "nil_response", "loader returned nil response")
		return
	}
	resp = closeWriterAndAttachReceipt(ctx, writer, resp)

	status := http.StatusOK
	if !resp.Status.IsTerminal() {
		status = http.StatusAccepted
	}
	writeJSON(w, status, resp)
}

// runStreamingLoad drives a multi-pass [StreamingLoaderHandler].
func runStreamingLoad(w http.ResponseWriter, r *http.Request, impl StreamingLoaderHandler, lc *LoadContext, writer transfer.Writer) {
	ctx := r.Context()
	plan, err := impl.Plan(ctx, lc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "plan_failed", err.Error())
		return
	}
	if plan == nil || len(plan.Passes) == 0 {
		writeError(w, http.StatusInternalServerError, "empty_plan",
			"streaming loader returned an empty plan; need at least one pass")
		return
	}
	startedAt := time.Now().UTC()
	for i := range plan.Passes {
		passSpec := plan.Passes[i]
		if err := impl.Pass(ctx, lc, &passSpec); err != nil {
			writeError(w, http.StatusInternalServerError, "pass_failed",
				fmt.Sprintf("pass %q: %s", passSpec.Name, err.Error()))
			return
		}
	}

	// Build a synthetic LoadResponse — streaming handlers don't
	// return one directly. The receipt-attaching path adds the
	// platform-issued job_id from the writer's Close().
	resp := &LoadResponse{
		Status:    StatusRunning,
		StartedAt: startedAt,
	}
	resp = closeWriterAndAttachReceipt(ctx, writer, resp)
	status := http.StatusOK
	if !resp.Status.IsTerminal() {
		status = http.StatusAccepted
	}
	writeJSON(w, status, resp)
}

// closeWriterAndAttachReceipt finalizes the artifact and merges the
// resulting [transfer.Receipt] into the LoadResponse the kit handler
// returned. When the kit's response was already terminal-failed, the
// writer Close error (if any) is appended; otherwise the JobID and
// stats are attached and the status is set to running so the
// platform polls loader.status to track terminal state.
func closeWriterAndAttachReceipt(ctx context.Context, writer transfer.Writer, resp *LoadResponse) *LoadResponse {
	if writer == nil {
		return resp
	}
	receipt, err := writer.Close(ctx)
	if err != nil {
		// Preserve the kit's response shape but flag the failure.
		if resp == nil {
			resp = &LoadResponse{}
		}
		resp.Status = StatusFailed
		if resp.Error == "" {
			resp.Error = "transfer close: " + err.Error()
		} else {
			resp.Error = resp.Error + "; transfer close: " + err.Error()
		}
		return resp
	}
	if resp == nil {
		resp = &LoadResponse{}
	}
	if receipt != nil {
		resp.JobID = receipt.JobID
		// Status is "running" — the platform-side processing happens
		// async; the install / import workflow polls loader.status
		// (a platform tool) for terminal state.
		if resp.Status == "" {
			resp.Status = StatusRunning
		}
		// Attach receipt details to Custom for kit-author visibility.
		if resp.Stats == nil {
			resp.Stats = &LoadStats{}
		}
		if resp.Stats.Custom == nil {
			resp.Stats.Custom = make(map[string]any)
		}
		resp.Stats.Custom["transfer_job_id"] = receipt.JobID
		resp.Stats.Custom["transfer_content_hash"] = receipt.ContentHash
		resp.Stats.Custom["transfer_bytes"] = receipt.BytesWritten
		resp.Stats.Custom["transfer_entries"] = receipt.EntryCount
		resp.Stats.Custom["transfer_mode"] = string(receipt.Mode)
	}
	return resp
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
		slog.Error("loader: write response", "error", err, "status", status)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, &ErrorResponse{Code: code, Message: message})
}
