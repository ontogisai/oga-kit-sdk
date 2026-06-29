package connector

import (
	"time"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Binding is one (external_system, source_type) pair a connector serves. A
// single connector may declare N bindings; the platform routes/accounts each
// submitted record by its source_type, and the webhook ingress addresses a
// binding by ID.
type Binding struct {
	// ID is the connector-unique binding identifier (stable; used in the
	// internal webhook path and the platform IngressToken mapping).
	ID string `json:"id"`

	// ExternalSystem is the system of record this binding talks to
	// (e.g. "contract_wo_mgmt"). Matches transfer.CorrelationKey.ExternalSystem
	// for entities this binding emits with external-ref merge.
	ExternalSystem string `json:"external_system"`

	// SourceType selects the binding's record class (e.g. "wo_status_feed",
	// "bms_point_stream"). Informational on the platform side for routing +
	// observability.
	SourceType string `json:"source_type"`

	// Mode declares how this binding ingests: poll, webhook, or both.
	// Empty defaults to poll.
	Mode IngressMode `json:"mode,omitempty"`
}

// IngressMode declares how a binding receives changes.
type IngressMode string

const (
	// ModePoll — the connector pulls on a cadence via Sync.
	ModePoll IngressMode = "poll"

	// ModeWebhook — the external system pushes; the platform webhook ingress
	// forwards to the connector's internal /webhook.
	ModeWebhook IngressMode = "webhook"

	// ModeBoth — the binding supports poll and webhook.
	ModeBoth IngressMode = "both"
)

// pollEnabled reports whether the mode runs a poll loop.
func (m IngressMode) pollEnabled() bool {
	return m == ModePoll || m == ModeBoth || m == ""
}

// webhookEnabled reports whether the mode accepts webhook pushes.
func (m IngressMode) webhookEnabled() bool {
	return m == ModeWebhook || m == ModeBoth
}

// valid reports whether the mode is one of the recognized values (empty
// defaults to poll). Guards against a binding constructed with a typo'd mode
// that would otherwise be silently neither polled nor webhook-served.
func (m IngressMode) valid() bool {
	switch m {
	case "", ModePoll, ModeWebhook, ModeBoth:
		return true
	default:
		return false
	}
}

// SyncResult is what Sync returns for one poll batch.
type SyncResult struct {
	// NextCursor is the opaque cursor the server hands back to the next Sync
	// call for this binding (e.g. a delta token or high-watermark). Empty
	// means "no advance" — the next poll reuses the prior cursor.
	NextCursor string

	// HasMore signals more pages are immediately available; when true the
	// server calls Sync again right away instead of waiting for the next tick.
	HasMore bool

	// Emitted is the record count this batch produced (observability only).
	Emitted int
}

// Health is a per-binding health report.
type Health struct {
	// OK is true when the binding is connected and able to ingest.
	OK bool `json:"ok"`

	// Message is an optional human-readable detail (reason when not OK,
	// or progress info such as last successful sync).
	Message string `json:"message,omitempty"`

	// LastSyncAt is the time of the last successful poll/push for the binding.
	LastSyncAt *time.Time `json:"last_sync_at,omitempty"`
}

// Emitter bundles the two record-emission surfaces a connector may use within
// a single Sync / HandleWebhook call. Entities is always present; Timeseries is
// non-nil only when a TimeseriesSink is configured. The server closes
// Entities (commits the batch) after the call returns — the kit MUST NOT call
// Entities.Close itself.
type Emitter struct {
	// Entities emits vertices/edges through the transfer contract.
	Entities transfer.Writer

	// Timeseries emits fixed-shape measurements (Tier-C). nil when no Sink
	// is configured for the connector.
	Timeseries TimeseriesSink
}
