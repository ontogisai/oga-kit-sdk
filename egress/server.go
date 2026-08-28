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
	"sync/atomic"
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
// /healthz and /livez are deliberately UNPREFIXED: they are platform-wide
// sidecar conventions and must not be role-specific.
//
// /livez answers 200 as soon as the process is up and serving — it has no
// dependency on Connect or Health, and is what the platform's readiness and
// liveness probes target for this role (OGA-874). /healthz stays the
// external-dependency signal, backed by Component.Health; it is informational
// to the platform now, never the target of a probe that can kill the
// container. See the [Component] doc comment for the full contract.
//
// NOTE: there is no entity-types introspection path. GET /egress/entity-types
// was removed in SJ24K-8 (platform half: OGA-855) — its own contract note said
// the MANIFEST is authoritative, which is the argument against serving a second,
// partial description of the same thing. It had no production consumer, and
// OGA-846's ontology_sync lane made it wrong as well as unused: it reported
// entity types only, so it could not describe a component's catalog anchors. If
// something needs "what does this component support", read the manifest.
// There is one path PER RECORD KIND, and that is the whole point rather than a
// convenience: the two lanes' batch labels legitimately collide (a kit declares
// the same anchor in both lanes), so before the split a component had to infer the
// kind from the payload. See [OntologyTypeSyncer].
const (
	PathSync             = "/egress/sync"
	PathOntologySync     = "/egress/ontology-sync"
	PathRelationshipSync = "/egress/relationship-sync"
	PathHealthz          = "/healthz"
	PathLivez            = "/livez"
	PathTestConnection   = "/egress/test-connection"
)

// Lane labels for LOGS ONLY.
//
// Unexported deliberately. The lane is carried by the route and by which method
// the server calls — putting a lane vocabulary back into the exported surface
// would invite a component to branch on it, which is the coupling this split
// removes. These exist so a log line says which lane a batch belonged to.
const (
	laneLabelEntities      = "entities"
	laneLabelOntologyTypes = "ontology_types"
	laneLabelRelationships = "relationships"
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

	// Background Connect retry (OGA-874). When the INITIAL Connect attempt
	// fails, the SDK retries it on an exponential backoff until it succeeds or
	// the context ends, so a component recovers from a transient external
	// outage without an operator pressing anything and without every kit author
	// hand-rolling their own re-probe loop.
	//
	// Zero values apply defaultConnectRetryInitialDelay /
	// defaultConnectRetryMaxDelay. Set DisableConnectRetry to opt out entirely
	// and own recovery yourself (e.g. a component that re-probes inside its own
	// Health, as coreegress.Component does).
	ConnectRetryInitialDelay time.Duration
	ConnectRetryMaxDelay     time.Duration
	DisableConnectRetry      bool
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

// ListenAndServe runs an egress component: it binds the HTTP listener, calls
// Connect, serves the sync/health/livez routes, and blocks until ctx is
// cancelled or SIGTERM/SIGINT arrives, then shuts the HTTP server down
// gracefully.
//
// Connect is called AFTER the listener is up (OGA-874). A Connect failure is
// logged and left for the component's own Health(ctx) to reflect — it no
// longer aborts startup. See the [Component] doc comment for the full
// contract; the rationale (container readiness must never depend on
// external-system reachability) is recorded in
// .kiro/specs/sidecar-external-dependency-health/design.md.
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

	s := &server{cfg: cfg, impl: impl}
	// Set BEFORE the listener starts accepting, so there is no instant at which
	// a push could be accepted ahead of the initial Connect attempt.
	s.connectPending.Store(true)
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

	// Connect AFTER the listener is up. A failure here is a recorded health
	// fact, not a startup abort — /livez already answers 200, and /healthz
	// reflects whatever the component's own Health(ctx) reports (a component
	// with no cached-health tracking of its own answers unhealthy until
	// Connect is retried, e.g. via the test-connection endpoint below).
	//
	// The push lanes are gated 503 for the duration of this call: the listener
	// is already accepting, so without the gate a batch could arrive before the
	// component holds the credentials Connect establishes. Cleared in a defer
	// so a panicking Connect cannot strand the component refusing every push.
	connectFailed := false
	func() {
		defer s.connectPending.Store(false)
		if err := impl.Connect(runCtx); err != nil {
			cfg.Logger.Error("egress component: initial connect failed; serving in a degraded state",
				"error", err)
			connectFailed = true
			// No return — the process keeps running and serving /livez + /healthz.
		}
	}()

	// Retry ONLY when the initial attempt failed. A component whose Connect
	// succeeded is never called again, which is what keeps the happy path
	// byte-identical to the original "called ONCE at startup" contract.
	if connectFailed && !cfg.DisableConnectRetry {
		go retryConnect(runCtx, cfg, impl)
	}

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

	// connectPending is true from the moment ListenAndServe binds the listener
	// until the initial Connect attempt has COMPLETED (whether it succeeded or
	// failed). While it is set, the push lanes answer 503 rather than accepting
	// a batch (OGA-874).
	//
	// This exists because binding before Connect (the OGA-874 fix) opened a
	// window that could not exist before: the listener is up, so /egress/sync
	// is reachable, while the component may not yet hold the credentials or
	// session Connect establishes. Accepting a batch there would produce
	// exactly the per-entity failure storm the ORIGINAL connect-before-listen
	// ordering existed to prevent — so the ordering change must not cost that
	// guarantee. Requirement 18 (a component that never fails Connect observes
	// no behavior change) depends on this gate.
	//
	// The ZERO VALUE is deliberately "not pending": a server constructed
	// directly (tests, or any future in-process embedding) has no Connect
	// lifecycle to wait on, so it behaves exactly as it did before this field
	// existed. Only ListenAndServe opts into the gate.
	connectPending atomic.Bool
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathSync, s.handleSync)
	mux.HandleFunc("POST "+PathOntologySync, s.handleOntologySync)
	mux.HandleFunc("POST "+PathRelationshipSync, s.handleRelationshipSync)
	mux.HandleFunc("GET "+PathHealthz, s.handleHealth)
	mux.HandleFunc("GET "+PathLivez, s.handleLivez)
	mux.HandleFunc("POST "+PathTestConnection, s.handleTestConnection)
	return mux
}

// pushFunc is the per-lane call. Everything around it — decode, the body cap, the
// Batch, verdict normalization, the response and the error mapping — is
// payload-agnostic and lives in servePush, so the two lanes cannot drift in how
// they treat a throttle, an empty batch or a malformed verdict.
type pushFunc func(ctx context.Context, req *SyncRequest, b *Batch) error

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	s.servePush(w, r, laneLabelEntities, s.impl.Sync)
}

// handleOntologySync serves the ontology lane, or reports that this component does
// not implement it.
func (s *server) handleOntologySync(w http.ResponseWriter, r *http.Request) {
	syncer, ok := s.impl.(OntologyTypeSyncer)
	if !ok {
		// Checked BEFORE decoding: the answer cannot depend on the body, and a
		// malformed body on a lane this component does not serve should report the
		// more fundamental problem rather than a decode error.
		//
		// 501 rather than 404, deliberately. A 404 is indistinguishable from a
		// wrong base URL, a stale gateway route or a path typo, which makes it
		// useless as a capability signal — the platform could not tell "this
		// component does not serve the lane" from "I am talking to the wrong
		// thing". 501 says the route exists and the behavior does not.
		//
		// Reaching here at all is a DEPLOYMENT MISMATCH, not a platform bug: the
		// platform pushes this lane only because the kit's manifest declared an
		// ontology_sync block, so the running image is out of step with the
		// manifest it was installed with. Hence ERROR, and hence naming the
		// interface and the compile-time assertion in the response.
		const msg = "this component does not serve the ontology lane: it does not implement " +
			"egress.OntologyTypeSyncer. The manifest declares an ontology_sync block, so the " +
			"running image is out of step with it — implement SyncOntologyTypes, and pin the " +
			"signature with `var _ egress.OntologyTypeSyncer = (*yourComponent)(nil)`"
		s.cfg.Logger.Error("egress ontology push rejected: component does not implement OntologyTypeSyncer",
			"path", PathOntologySync, "lane", laneLabelOntologyTypes)
		http.Error(w, "egress: "+msg, http.StatusNotImplemented)
		return
	}
	s.servePush(w, r, laneLabelOntologyTypes, syncer.SyncOntologyTypes)
}

// handleRelationshipSync serves the relationships lane, or reports that this
// component does not implement it.
//
// The relationships lane carries a DIFFERENT request/batch shape
// (RelationshipSyncRequest / RelationshipBatch, not SyncRequest / Batch), so
// unlike handleOntologySync it cannot delegate to servePush — that function's
// pushFunc signature is entity/ontology-shaped. serveRelationshipPush below is
// its exact counterpart for this lane, sharing the same gating and logging
// behavior by construction rather than by copying it correctly twice.
func (s *server) handleRelationshipSync(w http.ResponseWriter, r *http.Request) {
	syncer, ok := s.impl.(RelationshipSyncer)
	if !ok {
		// See handleOntologySync for why this is 501, checked before decoding,
		// and logged at ERROR — the same reasoning applies verbatim, substituting
		// "relationships_sync" for "ontology_sync" and RelationshipSyncer for
		// OntologyTypeSyncer.
		const msg = "this component does not serve the relationships lane: it does not implement " +
			"egress.RelationshipSyncer. The manifest declares a relationships_sync block, so the " +
			"running image is out of step with it — implement SyncRelationships, and pin the " +
			"signature with `var _ egress.RelationshipSyncer = (*yourComponent)(nil)`"
		s.cfg.Logger.Error("egress relationship push rejected: component does not implement RelationshipSyncer",
			"path", PathRelationshipSync, "lane", laneLabelRelationships)
		http.Error(w, "egress: "+msg, http.StatusNotImplemented)
		return
	}
	s.serveRelationshipPush(w, r, syncer.SyncRelationships)
}

// relationshipPushFunc is the relationships-lane call, mirroring pushFunc.
type relationshipPushFunc func(ctx context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error

// serveRelationshipPush is servePush's exact counterpart for the relationships
// lane: same connectPending gate, same decode-then-homogeneity-then-dispatch
// order, same malformed-verdict ERROR logging, same throttle/error mapping via
// writeSyncError (which is response-shape-agnostic — it only ever reads the
// request's batch_id/entity_type-equivalent fields for logging, so it is
// reused as-is below with the relationship request's fields substituted in a
// throwaway SyncRequest-shaped log context).
func (s *server) serveRelationshipPush(w http.ResponseWriter, r *http.Request, push relationshipPushFunc) {
	if s.connectPending.Load() {
		const msg = "component is still completing its initial Connect; retry shortly"
		s.cfg.Logger.Warn("egress push rejected: initial connect still in flight", "lane", laneLabelRelationships)
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}

	req, err := s.decodeRelationshipSync(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Relationships) == 0 {
		s.writeJSON(w, http.StatusOK, SyncResponse{Results: []SyncResult{}})
		return
	}

	b := newRelationshipBatch(req.Relationships)
	if syncErr := push(r.Context(), req, b); syncErr != nil {
		s.writeRelationshipSyncError(w, req, syncErr)
		return
	}

	results, defects := b.Results()
	for _, d := range defects {
		s.cfg.Logger.Error("egress component returned a malformed verdict; normalized to a per-relationship failure",
			"batch_id", req.BatchID, "lane", laneLabelRelationships, "predicate", req.Predicate, "defect", d)
	}
	s.writeJSON(w, http.StatusOK, SyncResponse{Results: results})
}

// decodeRelationshipSync reads and validates one relationships-lane push body.
// Mirrors decodeSync's rules — unknown fields accepted (forward-compat), a
// required identity field, homogeneity enforced, batch_id absence logged not
// rejected — substituting Predicate for EntityType as the homogeneity label.
func (s *server) decodeRelationshipSync(w http.ResponseWriter, r *http.Request) (*RelationshipSyncRequest, error) {
	var req RelationshipSyncRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fmt.Errorf("push body exceeds %d bytes", s.cfg.MaxRequestBytes)
		}
		return nil, fmt.Errorf("decode push body: %w", err)
	}
	if req.Predicate == "" {
		return nil, errors.New("predicate is required")
	}
	// Homogeneity: one predicate per call, exactly like EntityType's rule on
	// the other two lanes.
	for i := range req.Relationships {
		if p := req.Relationships[i].Predicate; p != "" && p != req.Predicate {
			return nil, fmt.Errorf(
				"batch is not homogeneous: relationships[%d].predicate = %q, request predicate = %q",
				i, p, req.Predicate)
		}
	}
	if req.BatchID == "" {
		s.cfg.Logger.Warn("egress push carries no batch_id; redelivery cannot be deduplicated",
			"predicate", req.Predicate, "mode", string(req.Mode))
	}
	return &req, nil
}

// writeRelationshipSyncError is writeSyncError's counterpart for the
// relationships lane — same status mapping (throttle → 429 with Retry-After,
// else 500), logged with the relationship batch's own identity fields instead
// of a SyncRequest's.
func (s *server) writeRelationshipSyncError(w http.ResponseWriter, req *RelationshipSyncRequest, err error) {
	var te *ThrottleError
	if errors.As(err, &te) && te.After > 0 {
		s.cfg.Logger.Warn("egress batch throttled by external system",
			"batch_id", req.BatchID, "lane", laneLabelRelationships, "predicate", req.Predicate,
			"retry_after", te.After.String(), "error", err)
		w.Header().Set("Retry-After", strconv.Itoa(int(te.After.Round(time.Second).Seconds())))
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	s.cfg.Logger.Error("egress batch failed",
		"batch_id", req.BatchID, "lane", laneLabelRelationships, "predicate", req.Predicate,
		"mode", string(req.Mode), "relationships", len(req.Relationships), "error", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// servePush is one push, whichever lane it belongs to.
func (s *server) servePush(w http.ResponseWriter, r *http.Request, lane string, push pushFunc) {
	// Checked BEFORE decoding, and before any component call: until the initial
	// Connect attempt has completed the component may hold no credentials, so
	// accepting this batch would fail it one entity at a time. 503 (not 4xx) so
	// the platform treats it as retryable — the condition clears on its own
	// within one Connect call. See server.connectPending.
	if s.connectPending.Load() {
		const msg = "component is still completing its initial Connect; retry shortly"
		s.cfg.Logger.Warn("egress push rejected: initial connect still in flight", "lane", lane)
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}

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
	if syncErr := push(r.Context(), req, b); syncErr != nil {
		s.writeSyncError(w, req, lane, syncErr)
		return
	}

	results, defects := b.Results()
	for _, d := range defects {
		// ERROR, not Warn: each of these is a bug in this component that would
		// have cost the platform the whole batch had the SDK forwarded it.
		s.cfg.Logger.Error("egress component returned a malformed verdict; normalized to a per-entity failure",
			"batch_id", req.BatchID, "lane", lane, "entity_type", req.EntityType, "defect", d)
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

// writeSyncError maps a batch-wide push failure onto the status the platform's
// retry classifier expects. Shared by both lanes: an external system's
// backpressure and a batch-wide fault mean the same thing whichever kind of record
// was being pushed.
func (s *server) writeSyncError(w http.ResponseWriter, req *SyncRequest, lane string, err error) {
	var te *ThrottleError
	if errors.As(err, &te) && te.After > 0 {
		s.cfg.Logger.Warn("egress batch throttled by external system",
			"batch_id", req.BatchID, "lane", lane, "entity_type", req.EntityType,
			"retry_after", te.After.String(), "error", err)
		// Seconds form: the platform accepts either permitted Retry-After form,
		// and a delay in seconds cannot be misread across a clock skew the way an
		// HTTP date can.
		w.Header().Set("Retry-After", strconv.Itoa(int(te.After.Round(time.Second).Seconds())))
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	s.cfg.Logger.Error("egress batch failed",
		"batch_id", req.BatchID, "lane", lane, "entity_type", req.EntityType,
		"mode", string(req.Mode), "entities", len(req.Entities), "error", err)
	// 500: a batch-wide fault is transient as far as the platform is concerned,
	// so it retries the batch with the SAME batch_id. Per-entity failures must
	// NOT come through here — they belong on the Batch, so the rest of the batch
	// keeps its correlations.
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := s.impl.Health(r.Context())
	code := http.StatusOK
	if !h.OK {
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, h)
}

// handleLivez answers 200 as soon as the process is up — it has no dependency
// on Connect or Health(ctx) at all. This is the endpoint the platform's K8s
// readiness/liveness probes target for this role (OGA-874): a container that
// can answer this is doing its job of being a process, regardless of whether
// its external dependency is currently reachable.
func (s *server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleTestConnection forces a fresh connectivity check against the external
// system, bypassing any cache/throttle the component applies to Health, and
// returns the resulting Health. It introduces no other side effect — no sync,
// no data push (Correctness Property P3 in the design doc).
//
// A component implementing the optional [Prober] interface is asked to force
// a fresh probe; otherwise the server falls back to a plain Health(ctx) call,
// which may return a cached verdict — best-effort, since the SDK cannot force
// a cache bypass it was never told how to perform.
func (s *server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	var h Health
	if p, ok := s.impl.(Prober); ok {
		h = p.TestConnection(r.Context())
	} else {
		h = s.impl.Health(r.Context())
	}
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
