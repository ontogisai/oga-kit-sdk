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
	// external system(s). Called once before any Sync/HandleWebhook. Returning
	// an error aborts startup.
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
