package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ontogisai/oga-kit-sdk/kitlog"
)

// Endpoint paths. Everything the egress contract owns is namespaced under
// /egress/ so it cannot collide with the loader contract's /load, /jobs,
// /formats or a source connector's /webhook — one image may serve more than one
// role, and an unprefixed /sync would make it ambiguous which contract a request
// belongs to.
//
// /healthz is deliberately UNPREFIXED: it is the platform-wide sidecar
// convention the health monitor probes and must not be role-specific.
const (
	PathSync        = "/egress/sync"
	PathEntityTypes = "/egress/entity-types"
	PathHealthz     = "/healthz"
)

// DefaultMaxRequestBytes caps a decoded push body. A batch is bounded by the
// manifest's batch_size, but entity properties are arbitrary, so the cap is
// generous and exists to bound memory rather than to police the platform.
const DefaultMaxRequestBytes int64 = 32 << 20 // 32 MiB

// Config tunes [ListenAndServe]. Zero values pick documented defaults.
type Config struct {
	// Port is the TCP port the component listens on. Required (the platform
	// allocates from the egress sidecar range and injects it).
	Port string

	// MaxRequestBytes caps the push body. Zero ⇒ DefaultMaxRequestBytes.
	MaxRequestBytes int64

	// Logger for lifecycle and component-defect messages. Defaults to
	// kitlog.Default() (the identity-seeded logger kitlog.Init installs).
	Logger *slog.Logger

	// HTTP server timeouts (sensible defaults applied when zero). WriteTimeout
	// is generous because a push waits on a third-party API.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

func (c *Config) defaults() {
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if c.Logger == nil {
		c.Logger = kitlog.Default()
	}
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
}

// ThrottleError asks the platform to back off before retrying this batch.
//
// Return one from [Component.Sync] when the EXTERNAL system signalled
// backpressure (its own 429, a quota window, a rate-limit header). The server
// answers 429 with a Retry-After header, which the platform honors — ignoring an
// external system's backpressure signal is how an integration gets itself
// rate-limited or blocked outright, so this is worth wiring through rather than
// hiding behind a generic error.
type ThrottleError struct {
	Err   error
	After time.Duration
}

// Throttled wraps err as a [ThrottleError] with the given backoff.
func Throttled(err error, after time.Duration) *ThrottleError {
	return &ThrottleError{Err: err, After: after}
}

func (e *ThrottleError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("egress: throttled, retry after %s", e.After)
	}
	return fmt.Sprintf("egress: throttled (retry after %s): %v", e.After, e.Err)
}

// Unwrap exposes the underlying cause.
func (e *ThrottleError) Unwrap() error { return e.Err }

// ListenAndServe runs an egress component: it calls Connect, serves the sync,
// entity-types and health routes, and blocks until ctx is cancelled or
// SIGTERM/SIGINT arrives, then shuts the HTTP server down gracefully.
func ListenAndServe(ctx context.Context, cfg *Config, impl Component) error {
	if cfg == nil {
		return errors.New("egress.ListenAndServe: nil config")
	}
	if cfg.Port == "" {
		return errors.New("egress.ListenAndServe: port is required")
	}
	if impl == nil {
		return errors.New("egress.ListenAndServe: impl is nil")
	}
	cfg.defaults()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Connect before listening. A component that cannot reach its external
	// system should fail startup rather than accept batches it would fail one
	// entity at a time — a failed run of 10,000 per-entity failures is far harder
	// for an operator to read than a sidecar that never came up.
	if err := impl.Connect(runCtx); err != nil {
		return fmt.Errorf("egress connect: %w", err)
	}

	s := &server{cfg: cfg, impl: impl}
	srv := &http.Server{
		Addr:              ":" + strings.TrimPrefix(cfg.Port, ":"),
		Handler:           s.mux(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		cfg.Logger.Info("egress component listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		cfg.Logger.Info("egress component shutdown signal", "signal", sig.String())
	case <-ctx.Done():
		cfg.Logger.Info("egress component context cancelled")
	case err := <-serveErr:
		return err
	}

	shutdownCtx, scancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer scancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		cfg.Logger.Error("egress component shutdown error", "error", err)
		return err
	}
	cfg.Logger.Info("egress component shutdown complete")
	return nil
}

// server holds the running component state.
type server struct {
	cfg  *Config
	impl Component
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathSync, s.handleSync)
	mux.HandleFunc("GET "+PathEntityTypes, s.handleEntityTypes)
	mux.HandleFunc("GET "+PathHealthz, s.handleHealth)
	return mux
}

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	req, err := s.decodeSync(w, r)
	if err != nil {
		// 400: the platform treats a non-429 4xx as PERMANENT and surfaces it
		// instead of retrying, which is right — retrying a request that could not
		// be understood just delays the operator seeing the real problem.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Entities) == 0 {
		s.writeJSON(w, http.StatusOK, SyncResponse{Results: []SyncResult{}})
		return
	}

	b := newBatch(req.Entities)
	if syncErr := s.impl.Sync(r.Context(), req, b); syncErr != nil {
		s.writeSyncError(w, req, syncErr)
		return
	}

	results, defects := b.Results()
	for _, d := range defects {
		// ERROR, not Warn: each of these is a bug in this component that would
		// have cost the platform the whole batch had the SDK forwarded it.
		s.cfg.Logger.Error("egress component returned a malformed verdict; normalized to a per-entity failure",
			"batch_id", req.BatchID, "entity_type", req.EntityType, "defect", d)
	}
	s.writeJSON(w, http.StatusOK, SyncResponse{Results: results})
}

// decodeSync reads and validates one push body.
func (s *server) decodeSync(w http.ResponseWriter, r *http.Request) (*SyncRequest, error) {
	var req SyncRequest
	// Unknown fields are ACCEPTED on purpose — the opposite of the manifest
	// parser, which is strict because a manifest is authored locally and a typo
	// should be caught. Here the sender is the platform: rejecting a field a
	// newer platform added would make every batch fail against an
	// already-deployed component, so the wire stays forward-compatible.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("push body exceeds %d bytes", s.cfg.MaxRequestBytes)
		}
		return nil, fmt.Errorf("decode push body: %w", err)
	}
	if req.EntityType == "" {
		return nil, errors.New("entity_type is required")
	}
	// Homogeneity is a contract guarantee, enforced here so a component may rely
	// on it: one entity_type per call means a component's handler is a switch on
	// the request, not a per-item dispatcher. A mixed batch is a caller bug, and
	// silently pushing it would map some entities with the wrong target shape.
	for i := range req.Entities {
		if et := req.Entities[i].EntityType; et != "" && et != req.EntityType {
			return nil, fmt.Errorf(
				"batch is not homogeneous: entities[%d].entity_type = %q, request entity_type = %q",
				i, et, req.EntityType)
		}
	}
	if req.BatchID == "" {
		// Not rejected: batch_id exists so a component MAY deduplicate a
		// redelivery, and a component that does not is still correct. Logged
		// because every platform push path sets it, so an empty one means
		// something upstream changed.
		s.cfg.Logger.Warn("egress push carries no batch_id; redelivery cannot be deduplicated",
			"entity_type", req.EntityType, "mode", string(req.Mode))
	}
	return &req, nil
}

// writeSyncError maps a batch-wide Sync failure onto the status the platform's
// retry classifier expects.
func (s *server) writeSyncError(w http.ResponseWriter, req *SyncRequest, err error) {
	var te *ThrottleError
	if errors.As(err, &te) && te.After > 0 {
		s.cfg.Logger.Warn("egress batch throttled by external system",
			"batch_id", req.BatchID, "entity_type", req.EntityType,
			"retry_after", te.After.String(), "error", err)
		// Seconds form: the platform accepts either permitted Retry-After form,
		// and a delay in seconds cannot be misread across a clock skew the way an
		// HTTP date can.
		w.Header().Set("Retry-After", strconv.Itoa(int(te.After.Round(time.Second).Seconds())))
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	s.cfg.Logger.Error("egress batch failed",
		"batch_id", req.BatchID, "entity_type", req.EntityType,
		"mode", string(req.Mode), "entities", len(req.Entities), "error", err)
	// 500: a batch-wide fault is transient as far as the platform is concerned,
	// so it retries the batch with the SAME batch_id. Per-entity failures must
	// NOT come through here — they belong on the Batch, so the rest of the batch
	// keeps its correlations.
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (s *server) handleEntityTypes(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.impl.(EntityTypeLister)
	if !ok {
		// 404 rather than an empty list: the endpoint is advisory and an empty
		// list would read as "this component supports nothing", which is a
		// different and misleading claim from "this component does not answer".
		http.Error(w, "entity-types introspection not implemented", http.StatusNotFound)
		return
	}
	types := lister.EntityTypes(r.Context())
	if types == nil {
		types = []string{}
	}
	s.writeJSON(w, http.StatusOK, EntityTypesResponse{EntityTypes: types})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := s.impl.Health(r.Context())
	code := http.StatusOK
	if !h.OK {
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, h)
}

func (s *server) writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The header is already written, so this can only be logged. It matters:
		// the platform sees a truncated body as a malformed response and discards
		// the batch, and without this line the cause would be invisible here.
		s.cfg.Logger.Error("egress response encode failed", "error", err)
	}
}
