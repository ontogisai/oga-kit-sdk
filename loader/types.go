package loader

import "time"

// JobStatus describes the lifecycle state of a load job. The same values are
// returned both inline from POST /load (synchronous loaders) and from
// GET /jobs/{id} (asynchronous loaders).
type JobStatus string

const (
	// StatusPending — job accepted but not yet processing. Rare; most loaders
	// transition straight to running.
	StatusPending JobStatus = "pending"

	// StatusRunning — job is in progress. The platform polls
	// GET /jobs/{id} until the status is terminal.
	StatusRunning JobStatus = "running"

	// StatusCompleted — job finished successfully. Stats are populated.
	StatusCompleted JobStatus = "completed"

	// StatusFailed — job finished with an error. Error field is populated.
	StatusFailed JobStatus = "failed"

	// StatusCancelled — job was cancelled, either by the platform or the
	// loader's own cancellation logic. Stats may be partial.
	StatusCancelled JobStatus = "cancelled"
)

// IsTerminal returns true when the job has reached a final state and the
// platform should stop polling.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// LoadRequest is the body of POST /load.
//
// All three top-level fields are scalar JSON; loader-specific tuning belongs
// inside Config (free-form map) so the contract surface stays stable as kits
// add new options.
type LoadRequest struct {
	// TenantID scopes the load to a single tenant. Required.
	TenantID string `json:"tenant_id"`

	// KitID identifies the domain kit issuing the load. Optional but
	// recommended for observability and quota accounting.
	KitID string `json:"kit_id,omitempty"`

	// SourceURI is the location to load from. Schemes are loader-specific
	// (e.g., file://, s3://, https://, sftp://). Required.
	SourceURI string `json:"source_uri"`

	// Format optionally narrows the format identifier when a loader
	// supports more than one (see /formats). When empty, the loader picks
	// the best match for the source URI.
	Format string `json:"format,omitempty"`

	// Config carries loader-specific parameters (chunk size, mapping rules,
	// validation toggles, etc.). Loaders document their accepted keys.
	Config map[string]any `json:"config,omitempty"`

	// PrincipalID is the user or service that initiated the import.
	// Recorded for audit but never used as a tenancy boundary.
	PrincipalID string `json:"principal_id,omitempty"`
}

// LoadResponse is the body of POST /load. The same shape is returned from
// GET /jobs/{id} so the platform can use one decoder for both paths.
//
// Synchronous loaders complete inside the POST and return Status=completed
// with Stats populated; async loaders return Status=running with a JobID
// and the platform polls /jobs/{id}.
type LoadResponse struct {
	// JobID is the loader-assigned identifier used to query the job later.
	// Required when Status is non-terminal.
	JobID string `json:"job_id,omitempty"`

	// Status reports where the job is in its lifecycle. See JobStatus.
	Status JobStatus `json:"status"`

	// StartedAt is when the loader accepted the job.
	StartedAt time.Time `json:"started_at,omitempty"`

	// CompletedAt is when the job reached a terminal state.
	CompletedAt time.Time `json:"completed_at,omitempty"`

	// Stats are populated on terminal status. Optional during running.
	Stats *LoadStats `json:"stats,omitempty"`

	// Error is populated when Status == failed. Plain string for
	// human display; structured error catalogs live on the platform side.
	Error string `json:"error,omitempty"`

	// Message is an optional human-readable progress message useful for
	// long-running async jobs ("processed 5000/12000 records").
	Message string `json:"message,omitempty"`
}

// LoadStats reports counts produced by a load. Fields are optional — loaders
// populate what makes sense for their domain.
type LoadStats struct {
	// VerticesCreated is the number of new entity vertices.
	VerticesCreated int `json:"vertices_created,omitempty"`

	// VerticesUpdated is the number of existing entity vertices updated.
	VerticesUpdated int `json:"vertices_updated,omitempty"`

	// EdgesCreated is the number of new relationship edges.
	EdgesCreated int `json:"edges_created,omitempty"`

	// EdgesUpdated is the number of existing relationship edges updated.
	EdgesUpdated int `json:"edges_updated,omitempty"`

	// RecordsRead is the total number of source records processed.
	RecordsRead int `json:"records_read,omitempty"`

	// RecordsSkipped is the number of source records the loader chose
	// to skip (filtered, duplicate, malformed but recoverable, etc.).
	RecordsSkipped int `json:"records_skipped,omitempty"`

	// Warnings are non-fatal issues recorded during the load. Capped by
	// the loader to keep response size bounded.
	Warnings []string `json:"warnings,omitempty"`

	// Custom holds loader-specific stats (e.g., {"buildings_imported": 12,
	// "ifc_version": "IFC4"}). Optional.
	Custom map[string]any `json:"custom,omitempty"`
}

// FormatsResponse is the body of GET /formats. Lists the format identifiers
// this loader understands. The platform uses this for capability discovery
// and validation when an operator selects a loader at import-submit time.
type FormatsResponse struct {
	// Formats is the list of supported format identifiers (e.g.,
	// "brick-campus-json", "ifc-step", "sap-pm-export"). At least one
	// entry MUST be present.
	Formats []string `json:"formats"`
}

// HealthResponse is the body of GET /healthz. Loaders return Status="ok"
// when ready to accept jobs. Anything else is treated as unhealthy.
type HealthResponse struct {
	// Status is "ok" when the loader is ready, otherwise a short
	// machine-friendly reason ("starting", "draining", "unavailable").
	Status string `json:"status"`

	// Message is an optional human-readable detail.
	Message string `json:"message,omitempty"`

	// Version is the loader binary version (Semver) for debugging.
	Version string `json:"version,omitempty"`
}

// ErrorResponse is returned by the loader for non-2xx responses. The
// platform decodes this and logs the loader-specific reason. Loaders
// that want to be observable always return a body matching this shape.
type ErrorResponse struct {
	// Code is a machine-readable token (kit-author-defined).
	Code string `json:"code,omitempty"`

	// Message is a human-readable description.
	Message string `json:"message"`

	// Details optionally carries structured context.
	Details map[string]any `json:"details,omitempty"`
}
