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
	"syscall"
	"time"

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

	// PollInterval is the cadence between poll batches per binding.
	// Default 30s. Bindings with no poll mode are not polled.
	PollInterval time.Duration

	// Logger for lifecycle messages. Defaults to slog.Default().
	Logger *slog.Logger

	// HTTP server timeouts (sensible defaults applied when zero).
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

func (c *Config) defaults() {
	if c.PollInterval == 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
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

// ListenAndServe runs a Source Connector: it calls Connect, starts one poll
// loop per poll-enabled binding, serves the internal webhook + health
// endpoints, and blocks until ctx is cancelled or SIGTERM/SIGINT arrives, then
// drains poll loops and shuts the HTTP server down gracefully.
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
	cfg.defaults()

	sink := cfg.Sink
	if sink == nil {
		sink = errSink{}
	}
	s := &server{cfg: cfg, impl: impl, sink: sink}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := impl.Connect(runCtx); err != nil {
		return fmt.Errorf("connector connect: %w", err)
	}

	bindings := impl.Bindings(runCtx)
	if len(bindings) == 0 {
		return errors.New("connector.ListenAndServe: connector declares no bindings")
	}
	byID := make(map[string]Binding, len(bindings))
	for _, b := range bindings {
		byID[b.ID] = b
	}
	s.bindings = byID

	// Poll loops (one per poll-enabled binding).
	var wg sync.WaitGroup
	for _, b := range bindings {
		if !b.Mode.pollEnabled() {
			continue
		}
		wg.Go(func() { s.pollBinding(runCtx, b) })
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
	cfg.Logger.Info("source connector shutdown complete")
	return nil
}

// server holds the running connector state.
type server struct {
	cfg      *Config
	impl     SourceConnector
	sink     TimeseriesSink
	bindings map[string]Binding
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/{binding}", s.handleWebhook)
	mux.HandleFunc("GET /webhook/{binding}", s.handleWebhookValidate)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

// pollBinding runs the poll loop for one binding until ctx is cancelled.
func (s *server) pollBinding(ctx context.Context, b Binding) {
	cursor := ""
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		// Drain immediately-available pages before waiting for the next tick.
		for {
			if ctx.Err() != nil {
				return
			}
			res, err := s.runSync(ctx, b, cursor)
			if err != nil {
				s.cfg.Logger.Warn("source connector sync failed",
					"binding", b.ID, "source_type", b.SourceType, "error", err)
				break // retry on next tick from the same cursor
			}
			if res != nil && res.NextCursor != "" {
				cursor = res.NextCursor
			}
			if res == nil || !res.HasMore {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runSync builds a writer, runs one Sync, and commits the batch on success.
// On error the writer is dropped (no commit), so a partial batch is never
// persisted and the next poll retries from the unchanged cursor.
func (s *server) runSync(ctx context.Context, b Binding, cursor string) (*SyncResult, error) {
	w, err := s.cfg.WriterFactory(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("writer factory: %w", err)
	}
	res, syncErr := s.impl.Sync(ctx, b, cursor, &Emitter{Entities: w, Timeseries: s.sink})
	if syncErr != nil {
		return nil, syncErr // drop the uncommitted writer
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
	payload, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	writer, err := s.cfg.WriterFactory(r.Context(), b)
	if err != nil {
		http.Error(w, "writer factory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if herr := s.impl.HandleWebhook(r.Context(), b, payload, &Emitter{Entities: writer, Timeseries: s.sink}); herr != nil {
		http.Error(w, "handle webhook: "+herr.Error(), http.StatusInternalServerError)
		return // drop uncommitted writer
	}
	if _, cerr := writer.Close(r.Context()); cerr != nil {
		http.Error(w, "commit: "+cerr.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
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
	health := s.impl.Health(r.Context())
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
