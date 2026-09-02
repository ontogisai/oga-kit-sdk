package connector

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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ontogisai/oga-kit-sdk/kitlog"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// WriterFactory builds a fresh [transfer.Writer] for one batch of a binding.
// The connector server calls it once per Sync / HandleWebhook invocation and
// commits (closes) the writer for the kit afterward. Production wiring returns
// a gateway-backed writer (transfer.NewDataWriter over an HTTPCommitClient).
type WriterFactory func(ctx context.Context, b Binding) (transfer.Writer, error)

// Config tunes [ListenAndServe]. Zero values pick documented defaults.
type Config struct {
	// Port is the TCP port the connector listens on for webhook + health.
	// Required (the internal sidecar port, 8500-8599 range in production).
	Port string

	// WriterFactory builds the per-batch entity writer. Required.
	WriterFactory WriterFactory

	// Sink is the Tier-C timeseries emit surface. Optional; when nil, a
	// connector that emits points fails loudly (no silent drop).
	Sink TimeseriesSink

	// WebhookMode selects how an inbound webhook delivery is processed.
	// Zero value is WebhookSync — the long-standing behaviour, unchanged.
	//
	// Set WebhookAsync when a delivery's work can outlive the request: a large
	// artifact download, a multi-thousand-record parse, anything that risks the
	// platform ingress proxy's read timeout (30s). The server then ACKs 202 and
	// runs the handler on a background worker.
	//
	// ⚠️ ASYNC MOVES RETRY OWNERSHIP TO THE CONNECTOR. In sync mode a handler
	// error becomes a 5xx and the upstream provider retries. Once 202 has been
	// sent that is impossible — the provider considers the delivery accepted, so
	// a failure can only be logged. An async connector MUST therefore either be
	// safe to lose a delivery (e.g. it re-reads a full snapshot on the next
	// trigger, so the next cycle repairs the gap) or own its own retry and
	// dedupe. It must also tolerate its handler running AFTER the response was
	// written, which means not capturing anything request-scoped.
	//
	// One class of failure is exempt, and an async connector should take the
	// exemption: implement [PayloadValidator] and a MALFORMED body is answered
	// 400 before the delivery is queued, so the caller is told its request was
	// bad instead of being told the delivery succeeded. That covers the failures
	// the caller can actually act on; what stays unreportable is everything
	// discovered later — a download that fails, a source that has moved on, a
	// commit the platform rejects.
	//
	// This is a Go-level choice on purpose, not a manifest field or an
	// environment variable: whether ACK-before-process is safe is a property of
	// the handler's code, so it belongs with the code rather than somewhere an
	// operator could flip it without touching the handler that has to satisfy it.
	WebhookMode WebhookMode

	// WebhookQueueDepth bounds the in-flight async queue. Ignored in sync mode.
	// Default defaultWebhookQueueDepth. A full queue answers 429 (never a silent
	// drop), so this is the knob for how much burst to absorb before shedding.
	WebhookQueueDepth int

	// ExtraRoutes registers kit-supplied routes on the connector's mux, keyed by
	// http.ServeMux pattern (e.g. "POST /trigger", "GET /metrics").
	//
	// It exists so needing one extra endpoint does not force a kit to abandon
	// this server and re-implement the whole contract by hand — which is what
	// the sj24k ontology-sync connector had to do, and how its webhook route
	// drifted from what the platform ingress actually posts to.
	//
	// A pattern that collides with a reserved contract path (the webhook,
	// health, livez and test-connection routes) is refused at startup, naming
	// the path: ServeMux panics on a duplicate registration, and a kit must not
	// be able to shadow a route the platform probes.
	ExtraRoutes map[string]http.Handler

	// PollInterval is the cadence between poll batches per binding.
	// Default 30s. Bindings with no poll mode are not polled.
	PollInterval time.Duration

	// Logger for lifecycle messages. Defaults to kitlog.Default() (the
	// identity-seeded logger installed by kitlog.Init()).
	Logger *slog.Logger

	// HTTP server timeouts (sensible defaults applied when zero).
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	// Background Connect retry (OGA-874). When the INITIAL Connect attempt
	// fails, the SDK retries it on an exponential backoff until it succeeds or
	// the context ends, so a connector recovers from a transient upstream
	// outage without an operator pressing anything and without every kit author
	// hand-rolling their own re-probe loop. This matters more for a connector
	// than for an egress component: its poll loops keep running through the
	// outage, so without the retry a connector that never established
	// credentials would poll fruitlessly forever.
	//
	// Zero values apply defaultConnectRetryInitialDelay /
	// defaultConnectRetryMaxDelay. Set DisableConnectRetry to opt out and own
	// recovery yourself.
	ConnectRetryInitialDelay time.Duration
	ConnectRetryMaxDelay     time.Duration
	DisableConnectRetry      bool
}

// WebhookMode selects the webhook processing strategy. See Config.WebhookMode.
type WebhookMode string

const (
	// WebhookSync processes the delivery inline and reports the outcome in the
	// response status. The zero value, so it stays the default.
	WebhookSync WebhookMode = ""

	// WebhookAsync ACKs 202 and processes on a background worker. Read the
	// retry-ownership warning on Config.WebhookMode before choosing it.
	WebhookAsync WebhookMode = "async"
)

// defaultWebhookQueueDepth bounds in-flight async deliveries when the kit does
// not choose. Small on purpose: a connector whose queue is deep enough to hide a
// backlog reports healthy while falling behind, and the 429 is the signal.
const defaultWebhookQueueDepth = 16

// valid reports whether the mode is recognized. Guards against a typo'd mode
// silently selecting sync (the zero value), which would reintroduce the very
// request-timeout the kit chose async to avoid.
func (m WebhookMode) valid() bool {
	switch m {
	case WebhookSync, WebhookAsync:
		return true
	default:
		return false
	}
}

// webhookJob is one queued async delivery.
type webhookJob struct {
	binding Binding
	payload []byte
}

func (c *Config) defaults() {
	if c.PollInterval == 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.WebhookQueueDepth <= 0 {
		c.WebhookQueueDepth = defaultWebhookQueueDepth
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

// ListenAndServe runs a Source Connector: it binds the HTTP listener, calls
// Connect, starts one poll loop per poll-enabled binding, serves the internal
// webhook + health + livez endpoints, and blocks until ctx is cancelled or
// SIGTERM/SIGINT arrives, then drains poll loops and shuts the HTTP server
// down gracefully.
//
// Connect is called AFTER the listener is up (OGA-874). A Connect failure is
// logged and left for the connector's own Health(ctx) to reflect — it no
// longer aborts startup. Bindings() is read regardless of Connect's outcome:
// a connector's binding declaration is static kit metadata and does not
// depend on connectivity, so the connector still registers its webhook routes
// and poll-loop scaffolding while its external system is down. See the
// [SourceConnector] doc comment for the full contract; the rationale is
// recorded in .kiro/specs/sidecar-external-dependency-health/design.md.
func ListenAndServe(ctx context.Context, cfg *Config, impl SourceConnector) error {
	if cfg == nil {
		return errors.New("connector.ListenAndServe: nil config")
	}
	if cfg.Port == "" {
		return errors.New("connector.ListenAndServe: port is required")
	}
	if cfg.WriterFactory == nil {
		return errors.New("connector.ListenAndServe: WriterFactory is required")
	}
	if impl == nil {
		return errors.New("connector.ListenAndServe: impl is nil")
	}
	if !cfg.WebhookMode.valid() {
		return fmt.Errorf("connector.ListenAndServe: WebhookMode %q is not one of %q | %q",
			cfg.WebhookMode, WebhookSync, WebhookAsync)
	}
	if err := validateExtraRoutes(cfg.ExtraRoutes); err != nil {
		return err
	}
	cfg.defaults()

	sink := cfg.Sink
	if sink == nil {
		sink = errSink{}
	}
	s := &server{cfg: cfg, impl: impl, sink: sink}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	bindings := impl.Bindings(runCtx)
	if len(bindings) == 0 {
		return errors.New("connector.ListenAndServe: connector declares no bindings")
	}
	byID, berr := validateBindings(bindings)
	if berr != nil {
		return berr
	}
	s.bindings = byID

	// Set BEFORE the listener starts accepting, so there is no instant at which
	// a webhook delivery could be processed ahead of the initial Connect attempt.
	s.connectPending.Store(true)

	// Async webhook worker. Started before the listener accepts, so a delivery
	// can never be enqueued with nothing draining the queue. The worker's context
	// is detached from runCtx (WithoutCancel): a queued job has already been ACKed
	// 202, so cancelling it at shutdown would abandon a delivery the provider
	// believes was accepted.
	if cfg.WebhookMode == WebhookAsync {
		s.webhookQueue = make(chan webhookJob, cfg.WebhookQueueDepth)
		s.webhookWorkerDone = make(chan struct{})
		go s.webhookWorker(context.WithoutCancel(runCtx))
		cfg.Logger.Info("source connector webhook mode: async",
			"queue_depth", cfg.WebhookQueueDepth)
	}

	server := &http.Server{
		Addr:              ":" + strings.TrimPrefix(cfg.Port, ":"),
		Handler:           s.mux(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		cfg.Logger.Info("source connector listening", "port", cfg.Port, "bindings", len(bindings))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Connect AFTER the listener is up and bindings are registered. A failure
	// here is a recorded health fact, not a startup abort — /livez already
	// answers 200, and /healthz reflects whatever the connector's own
	// Health(ctx) reports. The per-binding poll loops still start below: a
	// struggling Sync just keeps failing and retrying on its own cadence,
	// which is already-existing, already-correct behavior.
	//
	// Webhook delivery is gated 503 for the duration of this call (see
	// server.connectPending). Cleared in a defer so a panicking Connect cannot
	// strand the connector refusing every delivery.
	connectFailed := false
	func() {
		defer s.connectPending.Store(false)
		if err := impl.Connect(runCtx); err != nil {
			cfg.Logger.Error("source connector: initial connect failed; serving in a degraded state",
				"error", err)
			connectFailed = true
			// No return — the process keeps running and serving /livez + /healthz.
		}
	}()

	// Retry ONLY when the initial attempt failed, so a connector whose Connect
	// succeeded is never called again and the happy path keeps the original
	// "called once" contract.
	if connectFailed && !cfg.DisableConnectRetry {
		go retryConnect(runCtx, cfg, impl)
	}

	// Poll loops (one per poll-enabled binding).
	var wg sync.WaitGroup
	for _, b := range bindings {
		if !b.Mode.pollEnabled() {
			continue
		}
		wg.Go(func() { s.pollBinding(runCtx, b) })
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		cfg.Logger.Info("source connector shutdown signal", "signal", sig.String())
	case <-ctx.Done():
		cfg.Logger.Info("source connector context cancelled")
	case err := <-serveErr:
		cancel()
		wg.Wait()
		return err
	}

	// Drain: stop poll loops, then shut the HTTP server down.
	cancel()
	wg.Wait()

	shutdownCtx, scancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer scancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		cfg.Logger.Error("source connector shutdown error", "error", err)
		return err
	}

	// Drain the async queue AFTER Shutdown returns: no handler can enqueue any
	// more, so closing the channel is safe and the worker sees a fixed set of
	// jobs. Every one of them was ACKed 202, so we wait for them rather than
	// exiting — bounded by the same shutdown timeout, since a pathological
	// handler must not hang the process forever.
	if s.webhookQueue != nil {
		close(s.webhookQueue)
		select {
		case <-s.webhookWorkerDone:
		case <-shutdownCtx.Done():
			cfg.Logger.Error("source connector shutdown: async webhook queue did not drain in time; " +
				"deliveries already ACKed may be lost")
		}
	}

	cfg.Logger.Info("source connector shutdown complete")
	return nil
}

// server holds the running connector state.
type server struct {
	cfg      *Config
	impl     SourceConnector
	sink     TimeseriesSink
	bindings map[string]Binding

	// connectPending is true from the moment ListenAndServe binds the listener
	// until the initial Connect attempt has COMPLETED (whether it succeeded or
	// failed). While it is set, inbound webhook DELIVERY answers 503 rather
	// than being processed (OGA-874). Symmetric with the egress server's gate;
	// see that field's comment for the full rationale.
	//
	// The zero value is deliberately "not pending" so a directly-constructed
	// server (tests) behaves exactly as it did before this field existed — only
	// ListenAndServe opts into the gate.
	//
	// The webhook VALIDATION handshake (GET /webhook/{binding}) is deliberately
	// NOT gated: it echoes a challenge token back to the provider to prove
	// ownership of the endpoint, and many providers retry that only a fixed
	// number of times before disabling the subscription. Failing it during a
	// boot window would cost more than it protects, and unlike a delivery it
	// does not push data into the graph.
	connectPending atomic.Bool

	// webhookQueue is non-nil ONLY in async mode, and its nil-ness is what
	// handleWebhook branches on — so a directly-constructed server (tests) is
	// synchronous exactly as before this field existed.
	webhookQueue chan webhookJob
	// webhookWorkerDone is closed when the worker has drained the queue, so
	// shutdown can wait for in-flight deliveries it already ACKed.
	webhookWorkerDone chan struct{}
}

// validateBindings rejects empty or duplicate binding IDs and invalid modes
// before any poll loop or route is wired. Empty/duplicate IDs would otherwise
// collide in the route map and (for dups) start two poll loops for the same
// logical binding — double-ingest with independent cursors.
func validateBindings(bindings []Binding) (map[string]Binding, error) {
	byID := make(map[string]Binding, len(bindings))
	for i := range bindings {
		b := bindings[i]
		if b.ID == "" {
			return nil, fmt.Errorf("connector: binding[%d] has an empty ID", i)
		}
		if _, dup := byID[b.ID]; dup {
			return nil, fmt.Errorf("connector: duplicate binding ID %q", b.ID)
		}
		if !b.Mode.valid() {
			return nil, fmt.Errorf("connector: binding %q has invalid mode %q", b.ID, b.Mode)
		}
		byID[b.ID] = b
	}
	return byID, nil
}

// countingWriter wraps a transfer.Writer to track whether any record was
// emitted, so the server can skip committing an empty batch (a no-change poll
// or a no-op webhook must NOT produce an empty artifact every tick).
type countingWriter struct {
	transfer.Writer
	n int
}

func (c *countingWriter) WriteVertex(ctx context.Context, v transfer.Vertex) error {
	if err := c.Writer.WriteVertex(ctx, v); err != nil {
		return err
	}
	c.n++
	return nil
}

func (c *countingWriter) WriteEdge(ctx context.Context, e transfer.Edge) error {
	if err := c.Writer.WriteEdge(ctx, e); err != nil {
		return err
	}
	c.n++
	return nil
}

func (c *countingWriter) WriteEntityType(ctx context.Context, t transfer.EntityTypeDef) error {
	if err := c.Writer.WriteEntityType(ctx, t); err != nil {
		return err
	}
	c.n++
	return nil
}

func (c *countingWriter) WriteHierarchy(ctx context.Context, h transfer.HierarchyEntry) error {
	if err := c.Writer.WriteHierarchy(ctx, h); err != nil {
		return err
	}
	c.n++
	return nil
}

// PathLivez and PathTestConnection are unprefixed (livez, platform-wide
// convention) and connector-namespaced (test-connection, mirroring the egress
// contract's /egress/test-connection) respectively. See handleLivez and
// handleTestConnection (OGA-874).
const (
	PathLivez          = "/livez"
	PathTestConnection = "/connector/test-connection"
)

// reservedPaths are the contract paths the SDK owns. A kit ExtraRoutes pattern
// touching any of them is refused at startup: the platform probes and posts to
// these, so letting a kit shadow one would break the contract silently from the
// platform's side while looking fine in the kit.
var reservedPaths = map[string]bool{
	"/webhook/{binding}": true,
	"/healthz":           true,
	PathLivez:            true,
	PathTestConnection:   true,
}

// patternPath strips the optional leading METHOD from a ServeMux pattern, so
// "POST /healthz" and "/healthz" are recognized as the same path. Without this a
// collision check on the raw pattern would miss the method-qualified form and
// ServeMux would panic at registration instead.
func patternPath(pattern string) string {
	p := strings.TrimSpace(pattern)
	if i := strings.LastIndex(p, " "); i >= 0 {
		p = strings.TrimSpace(p[i+1:])
	}
	return p
}

// validateExtraRoutes rejects a kit route that is unusable or that would shadow
// a reserved contract path. ListenAndServe calls this BEFORE building the mux,
// because http.ServeMux PANICS on a duplicate registration — a collision has to
// surface as a startup error naming the path, not as a crash in a cluster.
func validateExtraRoutes(routes map[string]http.Handler) error {
	for pattern, h := range routes {
		if strings.TrimSpace(pattern) == "" {
			return errors.New("connector: ExtraRoutes has an empty pattern")
		}
		if h == nil {
			return fmt.Errorf("connector: ExtraRoutes[%q] has a nil handler", pattern)
		}
		if p := patternPath(pattern); reservedPaths[p] {
			return fmt.Errorf(
				"connector: ExtraRoutes[%q] collides with the reserved contract path %q — "+
					"the platform probes and delivers to that route, so a kit must not shadow it",
				pattern, p)
		}
	}
	return nil
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/{binding}", s.handleWebhook)
	mux.HandleFunc("GET /webhook/{binding}", s.handleWebhookValidate)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET "+PathLivez, s.handleLivez)
	mux.HandleFunc("POST "+PathTestConnection, s.handleTestConnection)

	// Kit-supplied routes last. Pre-validated by validateExtraRoutes on the
	// ListenAndServe path, so a reserved-path collision cannot reach the
	// registration below.
	for pattern, h := range s.cfg.ExtraRoutes {
		mux.Handle(pattern, h)
	}
	if n := len(s.cfg.ExtraRoutes); n > 0 {
		s.cfg.Logger.Info("source connector registered kit routes", "count", n)
	}
	return mux
}

// pollBinding runs the poll loop for one binding until ctx is cancelled.
func (s *server) pollBinding(ctx context.Context, b Binding) {
	cursor := ""
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		cursor = s.drain(ctx, b, cursor)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drain runs Sync repeatedly to consume all immediately-available pages for a
// binding, returning the advanced cursor. It stops at: ctx cancellation, a
// Sync error (retry next tick from the unchanged cursor), HasMore=false, or —
// the runaway guard — HasMore=true without the cursor advancing (which would
// otherwise spin a hot loop replaying the same page).
func (s *server) drain(ctx context.Context, b Binding, cursor string) string {
	for {
		if ctx.Err() != nil {
			return cursor
		}
		res, err := s.runSync(ctx, b, cursor)
		if err != nil {
			s.cfg.Logger.Warn("source connector sync failed",
				"binding", b.ID, "source_type", b.SourceType, "error", err)
			return cursor // retry on next tick from the same cursor
		}
		advanced := res != nil && res.NextCursor != "" && res.NextCursor != cursor
		if res != nil && res.NextCursor != "" {
			cursor = res.NextCursor
		}
		if res == nil || !res.HasMore {
			return cursor
		}
		if !advanced {
			s.cfg.Logger.Warn("source connector reported HasMore without advancing cursor; stopping drain",
				"binding", b.ID, "source_type", b.SourceType)
			return cursor
		}
	}
}

// runSync builds a writer, runs one Sync, and commits the batch on success.
// On error the writer is dropped (no commit), so a partial batch is never
// persisted and the next poll retries from the unchanged cursor. When Sync
// emits no entity records the writer is dropped too — a no-change poll must
// never commit an empty artifact.
func (s *server) runSync(ctx context.Context, b Binding, cursor string) (*SyncResult, error) {
	w, err := s.cfg.WriterFactory(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("writer factory: %w", err)
	}
	cw := &countingWriter{Writer: w}
	res, syncErr := s.impl.Sync(ctx, b, cursor, &Emitter{Entities: cw, Timeseries: s.sink})
	if syncErr != nil {
		return nil, syncErr // drop the uncommitted writer
	}
	if cw.n == 0 {
		return res, nil // nothing emitted — no empty commit
	}
	if _, cerr := w.Close(ctx); cerr != nil {
		return nil, fmt.Errorf("commit batch: %w", cerr)
	}
	return res, nil
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	b, ok := s.bindings[r.PathValue("binding")]
	if !ok || !b.Mode.webhookEnabled() {
		http.Error(w, "unknown or non-webhook binding", http.StatusNotFound)
		return
	}
	// Checked before reading the body: until the initial Connect attempt has
	// completed the connector may hold no credentials, so processing this
	// delivery could fail against the external system. 503 so the provider
	// retries — the condition clears within one Connect call. See
	// server.connectPending.
	//
	// This gate applies in BOTH webhook modes. In async mode the check has to
	// happen here rather than in the worker: once we have answered 202 the
	// provider will not retry, so a delivery accepted during the boot window
	// would be silently lost instead of redelivered.
	if s.connectPending.Load() {
		const msg = "connector is still completing its initial Connect; retry shortly"
		s.cfg.Logger.Warn("webhook delivery rejected: initial connect still in flight",
			"binding", b.ID)
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Payload validation runs INLINE, before the mode branch, so a malformed body
	// is answered 400 in both modes rather than 202-then-silence (async) or 500
	// (sync, where a handler cannot distinguish a bad request from its own
	// downstream failure). See PayloadValidator for why it must stay cheap.
	if v, ok := s.impl.(PayloadValidator); ok {
		if verr := v.ValidateWebhookPayload(r.Context(), b, payload); verr != nil {
			s.cfg.Logger.Warn("webhook delivery rejected: payload validation failed",
				"binding", b.ID, "error", verr)
			http.Error(w, "invalid payload: "+verr.Error(), http.StatusBadRequest)
			return
		}
	}

	if s.webhookQueue != nil {
		s.enqueueWebhook(w, b, payload)
		return
	}
	s.processWebhookSync(w, r, b, payload)
}

// enqueueWebhook is the ASYNC path: hand the delivery to the worker and answer
// 202 without waiting for it.
//
// A full queue is a 429, never a silent drop: the caller is told to retry, which
// is the only honest answer once we have decided not to process inline. Dropping
// would lose the delivery with a success status.
func (s *server) enqueueWebhook(w http.ResponseWriter, b Binding, payload []byte) {
	select {
	case s.webhookQueue <- webhookJob{binding: b, payload: payload}:
		w.WriteHeader(http.StatusAccepted)
		return
	default:
		s.cfg.Logger.Warn("webhook delivery rejected: in-flight queue is full",
			"binding", b.ID, "queue_depth", cap(s.webhookQueue))
		http.Error(w, "webhook queue is full; retry shortly", http.StatusTooManyRequests)
	}
}

// processWebhookSync is the SYNC path (the default): run the handler inline and
// report its outcome in the response status.
func (s *server) processWebhookSync(w http.ResponseWriter, r *http.Request, b Binding, payload []byte) {
	writer, err := s.cfg.WriterFactory(r.Context(), b)
	if err != nil {
		http.Error(w, "writer factory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cw := &countingWriter{Writer: writer}
	if herr := s.impl.HandleWebhook(r.Context(), b, payload, &Emitter{Entities: cw, Timeseries: s.sink}); herr != nil {
		http.Error(w, "handle webhook: "+herr.Error(), http.StatusInternalServerError)
		return // drop uncommitted writer
	}
	if cw.n == 0 {
		w.WriteHeader(http.StatusOK) // nothing emitted — no empty commit
		return
	}
	if _, cerr := writer.Close(r.Context()); cerr != nil {
		http.Error(w, "commit: "+cerr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// webhookWorker drains the async queue until it is closed.
//
// It exits on channel CLOSE, not on ctx cancellation, and that is deliberate:
// every job in the queue has already been ACKed with a 202, so abandoning one on
// shutdown loses a delivery the provider believes it handed over successfully.
// ListenAndServe closes the queue only after http.Server.Shutdown has returned,
// at which point no handler can enqueue any more.
func (s *server) webhookWorker(ctx context.Context) {
	defer close(s.webhookWorkerDone)
	for job := range s.webhookQueue {
		s.processWebhookAsync(ctx, job)
	}
}

// processWebhookAsync runs one queued delivery. The response is long gone, so
// the outcome can only be logged — see Config.WebhookMode on what that means for
// retry ownership.
func (s *server) processWebhookAsync(ctx context.Context, job webhookJob) {
	b := job.binding
	writer, err := s.cfg.WriterFactory(ctx, b)
	if err != nil {
		s.cfg.Logger.Error("async webhook: writer factory failed; delivery dropped",
			"binding", b.ID, "error", err)
		return
	}
	cw := &countingWriter{Writer: writer}
	if herr := s.impl.HandleWebhook(ctx, b, job.payload, &Emitter{Entities: cw, Timeseries: s.sink}); herr != nil {
		// Drop the uncommitted writer — identical to the sync path, so a partial
		// batch is never persisted.
		s.cfg.Logger.Error("async webhook: handler failed; batch dropped uncommitted",
			"binding", b.ID, "error", herr)
		return
	}
	if cw.n == 0 {
		return // nothing emitted — no empty commit
	}
	if _, cerr := writer.Close(ctx); cerr != nil {
		s.cfg.Logger.Error("async webhook: commit failed; batch dropped",
			"binding", b.ID, "error", cerr)
		return
	}
	s.cfg.Logger.Info("async webhook processed", "binding", b.ID, "records", cw.n)
}

func (s *server) handleWebhookValidate(w http.ResponseWriter, r *http.Request) {
	b, ok := s.bindings[r.PathValue("binding")]
	if !ok || !b.Mode.webhookEnabled() {
		http.Error(w, "unknown or non-webhook binding", http.StatusNotFound)
		return
	}
	if v, ok := s.impl.(ValidationHandler); ok {
		body, err := v.ValidateWebhook(r.Context(), b, r.URL.Query())
		if err != nil {
			http.Error(w, "validate: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeHealthMap(w, s.impl.Health(r.Context()))
}

// handleLivez answers 200 as soon as the process is up — it has no dependency
// on Connect, Bindings, or Health(ctx) at all. This is the endpoint the
// platform's K8s readiness/liveness probes target for this role (OGA-874): a
// container that can answer this is doing its job of being a process,
// regardless of whether its external system is currently reachable.
func (s *server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleTestConnection forces a fresh connectivity check against every
// binding's external system, bypassing any cache/throttle the connector
// applies to Health, and returns the resulting per-binding map. It introduces
// no other side effect — no Sync, no webhook processing (Correctness Property
// P3 in the design doc, symmetric with the egress contract).
//
// A connector implementing the optional [Prober] interface is asked to force
// a fresh probe of ALL bindings (a connector's Connect typically covers every
// binding's shared credential/session in one call); otherwise the server
// falls back to a plain Health(ctx) call, which may return a cached verdict.
func (s *server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	var health map[string]Health
	if p, ok := s.impl.(Prober); ok {
		health = p.TestConnection(r.Context())
	} else {
		health = s.impl.Health(r.Context())
	}
	s.writeHealthMap(w, health)
}

// writeHealthMap is the shared response-writer for both /healthz and
// /connector/test-connection: the "is every declared binding OK" verdict
// plus the per-binding detail, so the two endpoints cannot disagree on how a
// health map maps to an HTTP status.
func (s *server) writeHealthMap(w http.ResponseWriter, health map[string]Health) {
	allOK := len(health) > 0
	for _, b := range s.bindings {
		h, ok := health[b.ID]
		if !ok || !h.OK {
			allOK = false
			break
		}
	}
	code := http.StatusOK
	if !allOK {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(health)
}
