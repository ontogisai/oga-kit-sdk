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

// PayloadValidator is an optional interface a connector implements to reject a
// malformed webhook payload BEFORE the delivery is accepted, so the caller learns
// its request was bad instead of being told the delivery succeeded.
//
// It exists because of an asymmetry in the async contract. Once the server has
// answered 202 a handler error can only be logged — so without this seam a
// malformed body is indistinguishable, from the caller's side, from a delivery
// that worked. The upstream team then debugs a silent failure with a success status
// in hand. Validating here gives that one class of failure — "this payload is not
// something I can accept" — an honest answer in BOTH modes:
//
//	async: 400 instead of 202-then-silence
//	sync:  400 instead of the 500 a handler error maps to
//
// The sync improvement is real and not incidental: a handler cannot distinguish
// "your payload is bad" from "my downstream broke", so it reports both as 500.
//
// ⚠️ MUST BE CHEAP AND SIDE-EFFECT FREE. It runs INLINE IN THE REQUEST, ahead of
// the queue. Doing I/O here — resolving the payload's URL, calling the external
// system, touching the graph — reintroduces exactly the request-timeout exposure
// async mode exists to remove, while looking like validation. Parse the body and
// check its required fields; nothing else.
//
// Return an error ONLY when the payload itself is unacceptable. A transient
// condition (a dependency being down, a cache miss) must return nil and be dealt
// with by the handler, because a 400 tells the caller to change its request, and
// for a transient fault that advice is wrong.
//
// Implementing it does not excuse the handler from validating. The interface is
// optional, so the handler is the layer that must hold regardless; treat this as
// the fast, caller-facing half of a check the handler also makes.
//
// Distinct from ValidationHandler, which answers the provider's subscribe-time
// challenge on the webhook GET and carries no payload. This one is about a POSTed
// delivery's body.
type PayloadValidator interface {
	// ValidateWebhookPayload reports whether payload is acceptable for b. A
	// non-nil error is answered to the caller as 400 with the error's message.
	ValidateWebhookPayload(ctx context.Context, b Binding, payload []byte) error
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
