package connector

import "context"

// SourceConnector is the interface a kit author implements to expose a
// continuous ingress behavior over the connector HTTP contract. [ListenAndServe]
// wraps an implementation, taking care of the per-binding poll loops, the
// webhook + health routes, transfer-writer lifecycle, and graceful shutdown.
//
// Implementations must be safe for concurrent calls: the server runs one poll
// goroutine per binding and serves webhook requests from the HTTP server's
// goroutines. A connector that maintains shared state must serialize access.
type SourceConnector interface {
	// Bindings returns the (external_system, source_type) bindings this
	// connector serves. Called once at startup. At least one is required.
	Bindings(ctx context.Context) []Binding

	// Connect establishes credentials and verifies connectivity to the
	// external system(s). Called once, AFTER the HTTP server (including
	// /livez and /healthz) has already started listening and Bindings() has
	// been read.
	//
	// Returning an error no longer aborts the process (OGA-874) — it marks the
	// connector's initial health state as degraded, and the HTTP server keeps
	// serving (webhook routes stay registered, poll loops still start)
	// regardless. A connector that returns an error here MUST ensure its
	// Health(ctx) method reflects that failure (or a subsequent recovery) so
	// the platform's monitor and the console can observe it. An external
	// system being unreachable at boot is an expected, transient condition —
	// not a reason to crash-loop the container.
	//
	// Neither Sync nor HandleWebhook is called before this returns: poll loops
	// start after it, and webhook DELIVERY answers 503 until the initial
	// attempt has completed. The webhook VALIDATION handshake (GET) is not
	// gated — it proves endpoint ownership to the provider and does not push
	// data into the graph.
	//
	// ⚠️ Connect is called ONCE when it SUCCEEDS, but is RETRIED on an
	// exponential backoff when it fails, until it succeeds or the process ends
	// (Config.DisableConnectRetry opts out). So an implementation MUST be
	// idempotent: re-establishing credentials or a session on a later call has
	// to be safe, and must not leak the resources of a previous attempt. This
	// matters here because the poll loops keep running through an outage — a
	// connector that never established credentials would otherwise poll
	// fruitlessly forever. A connector whose Connect succeeds first time sees
	// no change at all.
	Connect(ctx context.Context) error

	// Sync runs one poll batch for a binding. The connector fetches changes
	// since cursor and emits records through em (Entities and/or Timeseries),
	// returning the next cursor. The server commits em.Entities for the kit
	// after Sync returns — kit code MUST NOT call em.Entities.Close.
	//
	// Only called for bindings whose Mode enables polling.
	Sync(ctx context.Context, b Binding, cursor string, em *Emitter) (*SyncResult, error)

	// HandleWebhook normalizes one inbound webhook payload for a binding and
	// emits the resulting records through em. The server commits em.Entities
	// after the call. Only called for bindings whose Mode enables webhooks.
	HandleWebhook(ctx context.Context, b Binding, payload []byte, em *Emitter) error

	// Health reports per-binding health, keyed by Binding.ID. A binding absent
	// from the map is treated as unknown/unhealthy by the server.
	Health(ctx context.Context) map[string]Health
}

// ValidationHandler is an optional interface a connector implements when an
// external system requires a subscribe-time webhook validation handshake
// (e.g. echoing a challenge token). When implemented, the connector server
// routes the validation GET to it; otherwise the server returns 200 with an
// empty body.
type ValidationHandler interface {
	// ValidateWebhook handles the provider's challenge for a binding and
	// returns the body to echo back (e.g. the challenge token).
	ValidateWebhook(ctx context.Context, b Binding, query map[string][]string) ([]byte, error)
}

// Prober is implemented by a connector whose Health(ctx) may serve a cached
// verdict. TestConnection forces a fresh check against every binding's
// external system, bypassing any cache/throttle, and returns the resulting
// per-binding Health map.
//
// Optional (OGA-874): a connector that does not implement this is probed via
// a plain Health(ctx) call instead by the server's
// POST /connector/test-connection handler — best-effort, since the SDK
// cannot force a cache bypass it was never told how to perform. Symmetric
// with egress.Prober.
type Prober interface {
	TestConnection(ctx context.Context) map[string]Health
}
