// Package outcomereport is the kit-side receiving contract for a platform
// sync-outcome report (OGA-917).
//
// # What a report is
//
// After the platform finishes an ingestion SUBMISSION (one loader.complete
// artifact) or an egress RUN (full or delta), it produces a generic report of
// what it did — counts, divergences, a bounded list of rejected records — and
// POSTs it to a kit sidecar that declared `receives_outcome_report: true` in its
// manifest. When the platform does NOT start a run for a publication it took in
// (auto-run off, egress paused) it sends a [StatusDeferred] report instead, so
// the source learns its publish is waiting rather than hearing nothing.
//
// Before this existed a submitter received {job_id, status: "running"} and was
// never told the outcome, and a run's result was readable only by an operator
// polling an admin route. A kit that fronts an external system could not close
// the loop without a human.
//
// # The boundary
//
// Everything here is in PLATFORM vocabulary. The platform does not know the
// partner's field names, its address, its credential or its signature scheme,
// and it never will — translating this envelope into an external system's shape
// is the kit's work, and so is deciding which reports matter. A kit is free to
// rename, aggregate, suppress or drop anything below.
//
// # Why it lives in its own package
//
// The same handler is mounted by two different sidecar roles — an egress-sync
// component and a source connector — which are separate packages with separate
// servers, configs and route tables. Declaring the shape and the path once here
// is what stops the two from drifting into subtly different contracts for the
// same platform call.
//
// # Delivery semantics a receiver must expect
//
//   - AT-LEAST-ONCE. The platform retries a transient failure and a redelivery
//     carries the SAME [Report.DeliveryID]. Deduplicate on it; do not assume a
//     report arrives once.
//   - Return 2xx only once you have durably accepted the report. The platform
//     treats any 2xx as final and will not send it again.
//   - Return 501 (or do not implement [Receiver]) to say you do not receive
//     reports at all — the platform dead-letters rather than burning its retry
//     budget.
//   - Any other 4xx is treated as permanently undeliverable and dead-lettered;
//     5xx, 429 and a transport failure are retried with backoff, honouring
//     Retry-After.
package outcomereport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// Path is the endpoint the platform POSTs a report to.
//
// ONE path for every receiving role, deliberately. The two roles namespace their
// own calls (/egress/sync, /connector/test-connection), but a report is the same
// platform call whoever receives it, and the platform must be able to address a
// receiver without first knowing which role it plays. It collides with neither
// role's namespace nor the loader's /load.
const Path = "/outcomereport"

// SchemaVersion is the envelope version this package understands.
//
// Present from the first release so a receiver can branch on a future change
// instead of sniffing for a field. A report carrying a HIGHER version than a
// receiver knows is still safe to accept: every future change to this envelope
// is additive, so unknown fields decode away and the fields below keep their
// meaning.
const SchemaVersion = 1

// Stage names the platform lane a report describes. These are PLATFORM lanes,
// never destinations — the platform has no vocabulary for a partner system.
type Stage string

const (
	// StageIngestion is one loader.complete submission (one artifact).
	StageIngestion Stage = "ingestion"
	// StageEgress is one egress run (full or delta), or a run not started
	// ([StatusDeferred]).
	StageEgress Stage = "egress"
)

// Status is a stage's terminal status.
type Status string

const (
	// StatusCompleted means the stage ran to completion. It does NOT mean
	// everything succeeded — check the rejected/failed buckets, which a
	// completed stage may legitimately carry.
	StatusCompleted Status = "completed"
	// StatusFailed means the stage itself broke. Counts should not be read;
	// [Report.Failure] carries the cause.
	StatusFailed Status = "failed"
	// StatusCancelled means an operator cancelled the run.
	//
	// Declared from the first release but NOT yet produced by the platform: the
	// cancellation path needs separate handling (tracked as OGA-939). It is here
	// so a receiver's parser needs no change when it starts arriving.
	StatusCancelled Status = "cancelled"
	// StatusDeferred (egress only) means a run was NOT started for a
	// publication the platform took in: auto-run is off for the tenant or the
	// platform, or the tenant's egress is paused. [Report.Deferral] says which,
	// and [Run.Publications] carries the one publication deferred.
	//
	// A deferred report carries NO counts and no record lists — nothing was
	// attempted. The publication is not lost: the next run of the component (a
	// Sync Now, a resume, a later auto run) delivers it and lists it in its own
	// report, so a receiver marks a publication as having reached the external
	// system from that later report, never from this one.
	//
	// [Report.Validate] has always accepted an unknown status, so a receiver
	// built before this constant existed already receives these reports.
	StatusDeferred Status = "deferred"
)

// DeferralReason says why a run was not started for a publication.
type DeferralReason string

const (
	// DeferralTenantAutoRunOff: the tenant's administrator turned auto-run off,
	// so a publication waits for a Sync Now.
	DeferralTenantAutoRunOff DeferralReason = "tenant_auto_run_off"
	// DeferralTenantPaused: the tenant's egress is paused; the resume starts the
	// run.
	DeferralTenantPaused DeferralReason = "tenant_paused"
	// DeferralPlatformAutoRunOff: the platform operator has auto-run switched
	// off for the deployment.
	DeferralPlatformAutoRunOff DeferralReason = "platform_auto_run_off"
)

// Deferral is the cause on a [StatusDeferred] report.
type Deferral struct {
	// Reason is the cause. Switch on the known values and treat an unknown one
	// as "deferred for a reason this receiver does not know"; the list may grow.
	Reason DeferralReason `json:"reason"`
}

// RunMode is how an egress run selected its records.
type RunMode string

const (
	// RunModeFull pushes every record of the declared population and withdraws
	// every gone record still correlated.
	RunModeFull RunMode = "full"
	// RunModeDelta pushes only what changed since the component's last
	// completed run, retries what an earlier run left outstanding, and withdraws
	// what was removed since then. Per type, a delta falls back to full when the
	// type has never completed (or its sync_generation changed), so a delta run
	// may contain full steps; the run's mode is what was requested.
	RunModeDelta RunMode = "delta"
)

// TriggerKind names what requested an egress run. Several requests that arrive
// while a run is waiting or running coalesce into one further run, so a run can
// carry several kinds.
type TriggerKind string

const (
	// TriggerManual: a person pressed Sync Now (or ran the admin command).
	TriggerManual TriggerKind = "manual"
	// TriggerIngestion: a data submission was taken in that wrote something.
	TriggerIngestion TriggerKind = "ingestion"
	// TriggerOntology: the tenant's ontology was activated.
	TriggerOntology TriggerKind = "ontology"
	// TriggerResume: the tenant's egress was resumed after a pause.
	TriggerResume TriggerKind = "resume"
	// TriggerRecovery: the component's external system became reachable again.
	TriggerRecovery TriggerKind = "recovery"
	// TriggerSchedule: the platform's daily backstop.
	TriggerSchedule TriggerKind = "schedule"
)

// PublicationKind names what a publication was.
type PublicationKind string

const (
	// PublicationData is a data submission (one loader.complete artifact).
	// [Publication.Ref] is its job id.
	PublicationData PublicationKind = "data"
	// PublicationOntology is an ontology snapshot the platform applied.
	// [Publication.Ref] is the change request id.
	PublicationOntology PublicationKind = "ontology"
)

// Publication is one publication by the source that an egress run covers: what
// lets a receiver say which of its publishes reached the external system.
type Publication struct {
	// Kind is data or ontology.
	Kind PublicationKind `json:"kind"`
	// Ref is the platform's identity for it: the data submission's job id, or
	// the ontology change request's id.
	Ref string `json:"ref"`
	// UpstreamRef is the source's own version, BYTE FOR BYTE: the
	// upstream_ref the submitter set on the data submission, or the upstream
	// version the ontology snapshot carried. Absent when the source supplied
	// none — never synthesised.
	UpstreamRef string `json:"upstream_ref,omitempty"`
	// TakenInAt is when the platform finished taking it in (RFC3339): the
	// submission's completion, or the snapshot's apply.
	TakenInAt string `json:"taken_in_at"`
}

// Count keys of the egress stage, all DENSE: every egress report carries each
// one, zero included (a [StatusDeferred] report carries none).
//
// The first six are counts of push verdicts. [CountWithdrawn] counts records the
// run retracted. [CountOutstanding] and [CountNeedsAttention] describe the
// component AFTER the run, not the run's work: how many records are still not
// in their latest state in the external system (every one is retried by the
// next run), and how many of those have failed long enough to need attention
// (see [FailedRecord.NeedsAttention]).
const (
	CountPushed         = "pushed"
	CountCreated        = "created"
	CountUpdated        = "updated"
	CountSkipped        = "skipped"
	CountFailed         = "failed"
	CountCorrelated     = "correlated"
	CountWithdrawn      = "withdrawn"
	CountOutstanding    = "outstanding"
	CountNeedsAttention = "needs_attention"
)

// Removal count keys of the ingestion stage, SPARSE like the rest of that
// stage's counts (absent means zero).
const (
	// CountVerticesRemoved: entities the submission said the source removed, now
	// tombstoned.
	CountVerticesRemoved = "vertices_removed"
	// CountVerticesRevived: tombstoned entities the submission brought back.
	CountVerticesRevived = "vertices_revived"
	// CountEdgesClosedByRemoval: relationships closed because one of their
	// entities was removed.
	CountEdgesClosedByRemoval = "edges_closed_by_removal"
)

// OntologyWait (ingestion, data submissions only) says whether the platform
// held the submission back for an ontology snapshot the same source was
// applying, and how the wait ended. The platform holds a data submission while
// the kit has an ontology change being applied, so the entities meet the
// classes of the same publication instead of being quarantined.
type OntologyWait string

const (
	// OntologyWaitNone: nothing was being applied; the submission was not held.
	OntologyWaitNone OntologyWait = "none"
	// OntologyWaitCompleted: the submission was held and released when the
	// apply finished.
	OntologyWaitCompleted OntologyWait = "completed"
	// OntologyWaitBound: the submission was held until the platform's bound
	// elapsed and was then processed anyway; entities of classes the apply had
	// not yet made available are quarantined (see
	// [Report.QuarantinedByType]).
	OntologyWaitBound OntologyWait = "bound"
)

// OntologyChangeStatus is the state of an ontology change reported on an
// ingestion report.
type OntologyChangeStatus string

// OntologyChangeAwaitingReview: the change waits for a platform reviewer. The
// platform does not hold a data submission for it; the submission's entities of
// the classes it would add are quarantined until it is approved and the source
// publishes again.
const OntologyChangeAwaitingReview OntologyChangeStatus = "awaiting_review"

// OntologyChange is an ontology change from the same source that was waiting
// for review when a data submission was processed, so a receiver can tell its
// users which assets are held and why.
type OntologyChange struct {
	// Status is the change's state. Today always awaiting_review.
	Status OntologyChangeStatus `json:"status"`
	// ChangeRequestID is the platform's id for the change.
	ChangeRequestID string `json:"change_request_id"`
	// UpstreamVersion is the source's version the change came from, byte for
	// byte. Absent when the source supplied none.
	UpstreamVersion string `json:"upstream_version,omitempty"`
	// AddedTypes are the entity types (source-native class IDs) the change
	// would add, bounded — see AddedTypesTruncated and AddedTypesCap. The
	// submission's entities of these types are counted in
	// [Report.QuarantinedByType].
	AddedTypes []string `json:"added_types,omitempty"`
	// AddedTypesTruncated reports that AddedTypes is a sample. Always sent.
	AddedTypesTruncated bool `json:"added_types_truncated"`
	// AddedTypesCap is the cap behind that flag.
	AddedTypesCap int `json:"added_types_cap,omitempty"`
}

// Disposition says what the platform did with a rejected record, because the
// remedies differ.
type Disposition string

const (
	// DispositionQuarantined means the record was set aside in the platform's
	// ingestion dead-letter store. It is reviewable and REPLAYABLE — an operator
	// can correct the cause and replay it.
	DispositionQuarantined Disposition = "quarantined"
	// DispositionHardFailure means the write itself failed. It is NOT replayable
	// from the platform side; the record must be resubmitted.
	DispositionHardFailure Disposition = "hard_failure"
)

// RecordKind distinguishes a vertex from an edge, which an upstream system
// usually models as different things and corrects differently.
type RecordKind string

const (
	// RecordKindEntity is a vertex / node record.
	RecordKindEntity RecordKind = "entity"
	// RecordKindRelationship is an edge record.
	RecordKindRelationship RecordKind = "relationship"
)

// Report is the envelope. One shape for both stages, discriminated by [Stage].
//
// Fields are populated per stage: Submission and the ingestion count block for
// [StageIngestion], Run and the egress blocks for [StageEgress]. A field that
// does not apply is omitted rather than zeroed, so a receiver can tell "not
// applicable" from "genuinely zero".
type Report struct {
	// SchemaVersion is the envelope version. Compare against [SchemaVersion].
	SchemaVersion int `json:"schema_version"`

	// Stage says which lane this describes, and therefore which of Submission /
	// Run is populated.
	Stage Stage `json:"stage"`

	// DeliveryID is stable across every redelivery of this same report. It is
	// the DEDUPLICATION KEY — the platform is at-least-once, so a receiver that
	// has already accepted this id should answer 2xx and do nothing.
	DeliveryID string `json:"delivery_id"`

	// TenantID is the serving tenant. Present on every report.
	//
	// A single endpoint receiving reports for several tenants MUST route on this
	// rather than on anything it inferred from the connection.
	TenantID string `json:"tenant_id"`

	// KitID is the kit the submitting / running sidecar belongs to.
	KitID string `json:"kit_id,omitempty"`

	// Status is the stage's terminal status.
	Status Status `json:"status"`

	// StartedAt / CompletedAt are RFC3339 platform timestamps.
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`

	// Submission is set for [StageIngestion].
	Submission *Submission `json:"submission,omitempty"`

	// Run is set for [StageEgress].
	Run *Run `json:"run,omitempty"`

	// Counts is the stage's tallies. The KEYS DIFFER BY STAGE — an ingestion
	// report counts vertices and edges, an egress report counts pushed records —
	// so this is a map rather than two structs: a receiver reads the keys its
	// stage defines and the platform can add a bucket without a schema break.
	//
	// TREAT AN ABSENT KEY AS ZERO. The two stages populate this differently and a
	// receiver must not read meaning into absence:
	//
	//	ingestion  SPARSE — a bucket that measured zero is OMITTED, and a bucket
	//	                    the dispatch does not measure at all (types_registered
	//	                    on a data dispatch) is also absent. The two are
	//	                    INDISTINGUISHABLE.
	//	egress     DENSE  — its buckets ([CountPushed] … [CountNeedsAttention])
	//	                    are ALWAYS present, including zero, because every run
	//	                    measures all of them. A [StatusDeferred] report has no
	//	                    counts at all: nothing was attempted. A report from a
	//	                    platform older than the withdrawn / outstanding /
	//	                    needs_attention buckets carries only the first six.
	//
	// So absence never means "not applicable"; on the ingestion lane it usually
	// just means zero. See [Report.Count].
	Counts map[string]int `json:"counts,omitempty"`

	// OntologyWait (ingestion, data submissions) says whether the platform held
	// the submission for an ontology apply from the same source, and how that
	// ended. Absent on an ontology submission and from an older platform.
	OntologyWait OntologyWait `json:"ontology_wait,omitempty"`

	// OntologyChange (ingestion) is an ontology change from the same source that
	// was waiting for a platform reviewer when this submission was processed.
	// The submission's entities of the classes it would add are quarantined
	// until it is approved and the source publishes again. Absent when none was
	// waiting.
	OntologyChange *OntologyChange `json:"ontology_change,omitempty"`

	// Deferral is the cause on a [StatusDeferred] report. Absent otherwise.
	Deferral *Deferral `json:"deferral,omitempty"`

	// QuarantinedByType breaks the VERTEX-stage ingestion quarantine down per
	// attempted ENTITY TYPE, so a receiver sees WHICH types were rejected rather
	// than only how many records were.
	//
	// Keyed by entity type. Its edge-stage counterpart is
	// [Report.QuarantinedEdgesByReason], which is keyed by ERROR CODE — a
	// different key space, so the two must never be merged into one tally.
	QuarantinedByType map[string]int `json:"quarantined_by_type,omitempty"`

	// QuarantinedEdgesByReason breaks the EDGE-stage ingestion quarantine down per
	// platform error code (an unresolvable endpoint, a cross-tenant endpoint, a
	// non-materializable relationship type).
	//
	// It exists because the vertex breakdown alone leaves an operator able to see
	// THAT relationships were set aside but not WHY — and a submission can fail
	// entirely at the edge stage, so a receiver reading only QuarantinedByType
	// would find it empty and conclude nothing was wrong.
	//
	// Keyed by ERROR CODE, unlike QuarantinedByType's entity types. Both are
	// complete tallies over their own stage; together they sum to the
	// records_quarantined count, with the edge stage's total also available
	// directly as the edges_quarantined count.
	QuarantinedEdgesByReason map[string]int `json:"quarantined_edges_by_reason,omitempty"`

	// RejectedRecords itemises ingestion records the platform did not persist.
	// Bounded — see RejectedRecordsTruncated and RejectedRecordsCap.
	RejectedRecords []RejectedRecord `json:"rejected_records,omitempty"`

	// RejectedRecordsTruncated reports that the list above is a SAMPLE because a
	// cap refused entries. Always sent, including false, so its absence means an
	// older platform rather than "nothing was truncated".
	RejectedRecordsTruncated bool `json:"rejected_records_truncated"`
	// RejectedRecordsCap is the configured cap behind that flag, so a receiver
	// never hardcodes it.
	RejectedRecordsCap int `json:"rejected_records_cap,omitempty"`

	// SkipReasons and FailReasons are the egress per-cause tallies. They are
	// COMPLETE — they sum to the corresponding count — even when the record
	// samples below are capped.
	SkipReasons map[string]int `json:"skip_reasons,omitempty"`
	FailReasons map[string]int `json:"fail_reasons,omitempty"`

	// FailedRecords is the egress per-record failure sample, bounded.
	//
	// Records that need attention ([FailedRecord.NeedsAttention]) come FIRST, so
	// the cap never hides one.
	//
	// FAILURES ONLY, and there is deliberately no skipped counterpart: a skip is
	// the normal path, so a per-record skip sample would be a bounded list of
	// the healthy case on every clean run. Use SkipReasons for skips — it carries
	// every cause with its count.
	FailedRecords []FailedRecord `json:"failed_records,omitempty"`

	// FailedRecordsTruncated / FailedRecordsCap bound FailedRecords.
	FailedRecordsTruncated bool `json:"failed_records_truncated"`
	FailedRecordsCap       int  `json:"failed_records_cap,omitempty"`

	// ReasonKeysTruncated reports that a reason tally hit its distinct-key cap,
	// so some causes are folded into a catch-all bucket. The COUNTS are still
	// complete and still sum; it is the attribution that is partial.
	ReasonKeysTruncated bool `json:"reason_keys_truncated"`
	ReasonKeysCap       int  `json:"reason_keys_cap,omitempty"`

	// PhaseErrors report an egress sync-phase advance that did not stick. A run
	// can be completed and still carry these; non-empty means "re-run this
	// component".
	PhaseErrors []string `json:"phase_errors,omitempty"`

	// Divergences carries the ingestion accounting divergences the platform
	// detected (declared-versus-decoded entry count, edge-count identity). A
	// divergence does NOT change the reported status — it is a guard surfacing a
	// self-report mismatch.
	Divergences map[string]int `json:"divergences,omitempty"`

	// DivergenceCount is the egress count of ontology types retired while their
	// external record still exists. Not an error and not retryable: a re-run
	// reports the same set.
	DivergenceCount int `json:"divergence_count,omitempty"`

	// Warnings are platform-authored operator-facing notes.
	Warnings []string `json:"warnings,omitempty"`

	// Failure carries the cause when Status is [StatusFailed]. A non-empty
	// rejected/failed bucket on a COMPLETED report is a different thing and is
	// not derivable from the counts — that is a stage that worked and found bad
	// records.
	Failure *Failure `json:"failure,omitempty"`
}

// Submission identifies one ingestion artifact.
type Submission struct {
	// JobID is the platform-issued job identifier, the same one loader.complete
	// returned and loader.status reports.
	JobID string `json:"job_id"`

	// Kind is the load kind ("data" or "ontology").
	Kind string `json:"kind,omitempty"`

	// SubmittedBy is the sidecar name that submitted the artifact. It is also
	// how the platform chose this receiver: an ingestion report goes to the
	// connector that submitted it.
	SubmittedBy string `json:"submitted_by,omitempty"`

	// UpstreamRef is the opaque handle the submitter set, echoed back
	// BYTE-FOR-BYTE. The platform never parsed it. Absent if none was supplied —
	// never synthesised.
	UpstreamRef string `json:"upstream_ref,omitempty"`
}

// Run identifies one egress run.
//
// A run is NOT keyed to a single ingestion submission: it covers whatever
// changed since the component's last run, so it may span several submissions or
// none. [Run.Publications] lists the source's publications it delivered, with
// each one's UpstreamRef, which is how a receiver correlates the two stages.
type Run struct {
	// RunID is unique per invocation. It identifies the invocation that STARTED
	// the run, so it is stable even for a long run the platform internally
	// continued across segments. Empty on a [StatusDeferred] report: no run was
	// started.
	RunID string `json:"run_id"`

	// Component is the egress component name, matching the manifest.
	Component string `json:"component"`

	// Mode is how the run selected its records. Absent on a [StatusDeferred]
	// report (no run started) and from an older platform, whose runs were all
	// full.
	Mode RunMode `json:"mode,omitempty"`

	// TriggerKinds are what requested the run, coalesced. Absent on a deferred
	// report and from an older platform.
	TriggerKinds []TriggerKind `json:"trigger_kinds,omitempty"`

	// Publications are the source's publications this report covers, oldest
	// first:
	//
	//	completed  the publications the run DELIVERED. Each publication is listed
	//	           as delivered by exactly one completed run.
	//	failed     the publications the run was planned for. They stay pending and
	//	           are listed again by the next run that completes.
	//	deferred   the one publication whose run was not started.
	//
	// Empty for a run that covered no publication (a Sync Now with nothing new
	// from the source, the daily backstop, a recovery run). Bounded — see
	// PublicationsCount, PublicationsTruncated and PublicationsCap.
	Publications []Publication `json:"publications,omitempty"`

	// PublicationsCount is how many publications the report covers, including
	// any the cap left out of Publications.
	PublicationsCount int `json:"publications_count"`

	// PublicationsTruncated reports that Publications is a sample. Always sent.
	PublicationsTruncated bool `json:"publications_truncated"`

	// PublicationsCap is the cap behind that flag.
	PublicationsCap int `json:"publications_cap,omitempty"`
}

// RejectedRecord is one ingestion record the platform did not persist.
type RejectedRecord struct {
	// RecordID is the identity the SUBMITTER supplied. It may be empty when the
	// record carried none — the entry is still reported, because a rejection
	// with no id is more useful than a silently dropped one.
	RecordID string `json:"record_id"`

	// Kind says whether this was an entity or a relationship.
	Kind RecordKind `json:"kind,omitempty"`

	// Disposition says whether it was quarantined (replayable) or a hard
	// failure (not).
	Disposition Disposition `json:"disposition,omitempty"`

	// ErrorCode is the platform error code, stable and greppable.
	ErrorCode string `json:"error_code,omitempty"`

	// Message is human-readable detail. Prose, not a contract — branch on
	// ErrorCode.
	Message string `json:"message,omitempty"`
}

// FailedRecord is one egress record the component reported as failed.
type FailedRecord struct {
	// ID is the platform entity or edge id.
	ID string `json:"id"`

	// Kind says whether this was an entity or a relationship.
	Kind RecordKind `json:"kind,omitempty"`

	// ReasonCode is the cause code, drawn from the component's own vocabulary.
	ReasonCode string `json:"reason_code,omitempty"`

	// Reason is human-readable detail.
	Reason string `json:"reason,omitempty"`

	// FirstFailedAt is when this record first failed (RFC3339), in this run or
	// an earlier one. A record keeps failing until the cause is fixed, and every
	// run retries it; this is what lets a receiver say "has not synced since
	// Tuesday" instead of repeating the same rejection as news. Absent from an
	// older platform.
	FirstFailedAt string `json:"first_failed_at,omitempty"`

	// Attempts is how many runs have failed this record, this one included.
	Attempts int `json:"attempts,omitempty"`

	// NeedsAttention marks a record that is not resolving on its own: it has
	// failed in at least a configured number of runs over at least a configured
	// time. The platform keeps retrying it either way.
	NeedsAttention bool `json:"needs_attention,omitempty"`
}

// Failure is the cause of a failed stage.
type Failure struct {
	// Code is the platform error code.
	Code string `json:"code,omitempty"`
	// Message is human-readable detail.
	Message string `json:"message,omitempty"`
}

// Receiver is the kit-side interface for accepting a report.
//
// Implement it on your component and pass it to the role's ListenAndServe; both
// egress and connector mount [Handler] for you when one is supplied. Pin the
// signature so a refactor cannot silently stop satisfying it:
//
//	var _ outcomereport.Receiver = (*yourComponent)(nil)
type Receiver interface {
	// ReceiveOutcomeReport accepts one report.
	//
	// Return nil ONLY once the report is durably accepted — the platform treats
	// 2xx as final and will not resend. Returning an error yields 500 and the
	// platform retries, so return one for a transient problem (your own store is
	// down) and nil for a report you have decided to ignore.
	//
	// The same report may arrive more than once with the same
	// [Report.DeliveryID]; deduplicate on it.
	ReceiveOutcomeReport(ctx context.Context, rep *Report) error
}

// DefaultMaxRequestBytes caps a decoded report body.
//
// A report is bounded by construction (every record list has a platform-side
// cap), so this is a backstop against a malformed or hostile body rather than a
// real constraint on a legitimate report.
const DefaultMaxRequestBytes = 4 << 20 // 4 MiB

// Config configures [Handler].
type Config struct {
	// Receiver accepts reports. When nil the handler answers 501 — see [Handler].
	Receiver Receiver

	// MaxRequestBytes caps the request body. Zero ⇒ [DefaultMaxRequestBytes].
	MaxRequestBytes int64

	// Logger is used for rejection and acceptance logging. Zero ⇒
	// slog.Default().
	Logger *slog.Logger
}

// (An exported ErrNotImplemented was declared here in v0.93.0-beta and REMOVED:
// nothing ever returned it. The absence of a receiver is reported over HTTP as
// 501 — see [Handler] — so there was no construction path to carry an error, and
// a kit matching on it with errors.Is would have been writing dead code against a
// doc comment that described behaviour the package did not have.)

// Handler returns the HTTP handler for [Path].
//
// A NIL Receiver yields 501 on every request rather than a 404 or a silent 200.
// That distinction is load-bearing: the platform reads 501 as "this component
// does not receive reports" and dead-letters immediately instead of burning its
// retry budget, whereas a 404 is indistinguishable from a misdeployed sidecar
// and a silent 200 would make the platform believe a report was accepted that
// nothing ever read.
//
// Reaching 501 at all is a DEPLOYMENT MISMATCH, not a platform bug: the platform
// only delivers here because the kit's manifest set receives_outcome_report,
// so the running image is out of step with the manifest it was installed with.
// Hence the ERROR log and the interface named in the response.
func Handler(cfg Config) http.HandlerFunc {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBytes := cfg.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRequestBytes
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Receiver == nil {
			const msg = "this component does not receive sync-outcome reports: it does not " +
				"implement outcomereport.Receiver. Its manifest sets receives_outcome_report, so " +
				"the running image is out of step with it — implement ReceiveOutcomeReport, and " +
				"pin the signature with `var _ outcomereport.Receiver = (*yourComponent)(nil)`"
			logger.Error("outcome report rejected: component does not implement Receiver",
				"path", Path)
			http.Error(w, "outcomereport: "+msg, http.StatusNotImplemented)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
		if err != nil {
			// A body that exceeded the cap, or a truncated read. Either way the
			// platform should not retry it unchanged, so this is a 400.
			logger.Error("outcome report rejected: unreadable body", "path", Path, "error", err)
			http.Error(w, "outcomereport: unreadable request body", http.StatusBadRequest)
			return
		}

		var rep Report
		if err := json.Unmarshal(body, &rep); err != nil {
			logger.Error("outcome report rejected: malformed JSON", "path", Path, "error", err)
			http.Error(w, "outcomereport: malformed JSON body", http.StatusBadRequest)
			return
		}

		// Validate only what a receiver cannot act without. A report missing its
		// tenant or delivery id cannot be routed or deduplicated, and retrying it
		// would not help — so 400, not 500.
		if err := rep.Validate(); err != nil {
			logger.Error("outcome report rejected: invalid report",
				"path", Path, "delivery_id", rep.DeliveryID, "error", err)
			http.Error(w, "outcomereport: "+err.Error(), http.StatusBadRequest)
			return
		}

		if err := cfg.Receiver.ReceiveOutcomeReport(r.Context(), &rep); err != nil {
			// 500 so the platform RETRIES. A receiver that wants a report dropped
			// returns nil; an error here means "I could not accept it".
			logger.Error("outcome report handler failed",
				"path", Path, "delivery_id", rep.DeliveryID,
				"stage", string(rep.Stage), "tenant_id", rep.TenantID, "error", err)
			http.Error(w, "outcomereport: receiver failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		logger.Info("outcome report accepted",
			"delivery_id", rep.DeliveryID, "stage", string(rep.Stage),
			"tenant_id", rep.TenantID, "status", string(rep.Status))
		w.WriteHeader(http.StatusNoContent)
	}
}

// Validate checks the fields a receiver cannot function without.
//
// Deliberately MINIMAL. An unknown Stage or Status is NOT an error: the envelope
// is additive and the platform may add a lane or a terminal state, and refusing
// one would turn a forward-compatible change into a delivery outage. A receiver
// that only handles known values should switch on them and ignore the rest.
func (r *Report) Validate() error {
	if r.DeliveryID == "" {
		return errors.New("delivery_id is required (it is the deduplication key)")
	}
	if r.TenantID == "" {
		return errors.New("tenant_id is required (a report cannot be routed without it)")
	}
	if r.Stage == "" {
		return errors.New("stage is required")
	}
	if r.Status == "" {
		return errors.New("status is required")
	}
	return nil
}

// IsTerminalFailure reports whether the stage itself broke, as opposed to
// completing and finding bad records.
//
// The distinction is not derivable from the counts, which is why it is offered
// here: a completed report with a non-empty rejected bucket means the stage
// worked, whereas a failed report's counts should not be read at all.
func (r *Report) IsTerminalFailure() bool { return r.Status == StatusFailed }

// Count returns a named count and whether the key was PRESENT.
//
// ⚠️ `false` does NOT mean "this stage does not measure that bucket". On the
// ingestion lane the platform omits a bucket that measured zero, so a present-zero
// and a not-measured bucket are indistinguishable here — `Count("edges_updated")`
// and `Count("types_registered")` both report `(0, false)` on a data dispatch
// whose edges happened not to change. Only the egress lane is dense enough for
// presence to carry information, and there every documented bucket is always
// present anyway.
//
// Use the bool to tell "the platform said zero" from "the platform said nothing"
// for LOGGING or diagnostics. Do NOT branch business logic on it, and do not read
// absence as "not applicable to this stage". For arithmetic, prefer
// [Report.CountOrZero].
//
// (An earlier release documented the opposite — that absence distinguished
// not-reported from zero. It never held for the ingestion lane, which is the lane
// that reports per-record detail, so the claim was withdrawn rather than the
// producer changed: emitting a dense ingestion map would mean asserting
// `vertices_created: 0` on an ontology dispatch, which measures no vertices at
// all.)
func (r *Report) Count(name string) (int, bool) {
	if r.Counts == nil {
		return 0, false
	}
	v, ok := r.Counts[name]
	return v, ok
}

// CountOrZero returns a named count, treating an absent key as zero.
//
// This is the right accessor for arithmetic and for display, because absence
// carries no information on the ingestion lane (see [Report.Count]). Reach for
// [Report.Count] only when the distinction between "said zero" and "said nothing"
// is itself what you are reporting.
func (r *Report) CountOrZero(name string) int {
	return r.Counts[name]
}

// String renders a one-line summary for logs.
func (r *Report) String() string {
	unit := "-"
	switch {
	case r.Submission != nil:
		unit = "job=" + r.Submission.JobID
	case r.Run != nil:
		unit = "run=" + r.Run.RunID
	}
	return fmt.Sprintf("outcomereport{stage=%s status=%s tenant=%s %s delivery=%s}",
		r.Stage, r.Status, r.TenantID, unit, r.DeliveryID)
}
