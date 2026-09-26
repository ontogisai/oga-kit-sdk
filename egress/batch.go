package egress

import (
	"fmt"
	"strings"
)

// Batch collects a component's per-entity verdicts for one push and builds a
// response the platform will accept.
//
// It exists because the platform's acceptance rule is strict and its failure
// mode is total: a reply that omits a requested id, repeats one, references an
// id that was not requested, carries an unrecognized outcome, or reports
// created/updated without an external record id is treated as MALFORMED, and
// the platform then discards the WHOLE batch and persists nothing
// (OGA-EGRS-DATA-2001). That is the right platform behavior — partially trusting
// such a reply risks writing one entity's external record id onto another — but
// it means a small bookkeeping slip in a component costs every correlation in
// the batch. Batch removes the bookkeeping.
//
// A Batch is NOT safe for concurrent use. The platform runs concurrent batches,
// not concurrent writers within one batch; if a component fans a single batch
// out across goroutines it must serialize its calls here.
type Batch struct {
	ids     []string            // requested ids, in request order
	known   map[string]struct{} // membership, for rejecting unrequested ids
	results map[string]SyncResult
	defects []string
	verb    laneVerb // which outcomes this batch's lane accepts
}

// newBatch builds a push-lane Batch for the entities of one request.
func newBatch(entities []Entity) *Batch { return newLaneBatch(verbPush, entities) }

// newLaneBatch builds a Batch whose lane accepts verb's outcomes.
func newLaneBatch(verb laneVerb, entities []Entity) *Batch {
	b := &Batch{
		verb:    verb,
		ids:     make([]string, 0, len(entities)),
		known:   make(map[string]struct{}, len(entities)),
		results: make(map[string]SyncResult, len(entities)),
	}
	for i := range entities {
		id := entities[i].ID
		if id == "" {
			continue
		}
		if _, dup := b.known[id]; dup {
			continue // one result per id; a repeated request id is answered once
		}
		b.known[id] = struct{}{}
		b.ids = append(b.ids, id)
	}
	return b
}

// Created records that the component created an external record for id.
// externalRecordID is REQUIRED — see [Batch.Updated].
func (b *Batch) Created(id, externalRecordID string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeCreated, ExternalRecordID: externalRecordID})
}

// Updated records that the component updated the existing external record for
// id.
//
// externalRecordID is REQUIRED for both Created and Updated: it is what the
// platform persists as the entity's correlation, and it is what makes the NEXT
// push an update instead of a duplicate create. Passing an empty value is a
// component bug and is downgraded to a failure — see [Batch.Results].
func (b *Batch) Updated(id, externalRecordID string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeUpdated, ExternalRecordID: externalRecordID})
}

// Withdrawn records that the component retracted the external record for id.
// It takes no external record id: there is no longer a record to address.
//
// Valid ONLY on a withdrawal lane ([EntityWithdrawer]). On a push lane it is
// normalized to a per-entity failure — see [Batch.Results].
//
// Report a record that was ALREADY absent as [Batch.Skipped] (or
// [Batch.SkippedReason]) rather than this: the platform releases the
// correlation either way, and the distinction is what the audit trail records.
func (b *Batch) Withdrawn(id string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeWithdrawn})
}

// Skipped records that id needs no external record, or is already current.
//
// This is a SUCCESS, and it is the right verdict for "nothing to do" — a run
// full of skips is a clean run. Do NOT use it to swallow a problem: a skipped
// entity is never correlated and never retried, so an error reported as a skip
// disappears permanently from the operator's view.
//
// It records NO reason, which is the honest verdict when there is none to give:
// "already current" needs no explanation. Prefer [Batch.SkippedReason] whenever
// the component knows WHY, because a bare skip reaches the operator as a number
// they cannot attribute — they cannot tell a deliberate no-op from a whole class
// of records silently not being pushed.
func (b *Batch) Skipped(id string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeSkipped})
}

// SkippedReason is [Batch.Skipped] with a cause the operator's run report can
// group by and read.
//
// It is a SEPARATE method rather than a required argument on Skipped for the
// reason the bare form documents: a genuine "nothing to do" has no cause, and
// forcing one would produce invented prose — which is worse than an honest
// absence, because it looks like an answer.
//
// code is the STABLE grouping key and detail is the human specifics; see
// [SyncResult.ReasonCode] for what makes a good code and which prefixes are
// reserved. Either may be empty: with neither this is exactly [Batch.Skipped].
func (b *Batch) SkippedReason(id, code, detail string) {
	b.record(SyncResult{
		ID:           id,
		Outcome:      OutcomeSkipped,
		ReasonCode:   b.reasonCode(id, code),
		ReasonDetail: strings.TrimSpace(detail),
	})
}

// Failed records a per-entity failure for id. reason reaches the operator's run
// report, so make it actionable; an empty reason is replaced with a placeholder
// rather than left blank.
//
// This fails THIS entity only — the rest of the batch is still correlated. For a
// batch-wide fault return an error from Sync instead, which lets the platform
// retry the whole batch.
//
// Prefer [Batch.FailedReason] when the component can classify the failure: the
// report groups failures by code, so prose alone tallies every differently-worded
// message as its own cause.
func (b *Batch) Failed(id, reason string) {
	code := ""
	if reason == "" {
		reason = "component reported a failure without a reason"
		// A stable code for the ONE case this method can classify by itself. The
		// placeholder prose says the same thing, but prose is not a grouping key.
		code = ReasonCodeNoReason
	}
	b.record(SyncResult{
		ID: id, Outcome: OutcomeFailed,
		Error: reason, ReasonCode: code, ReasonDetail: reason,
	})
}

// FailedErr is [Batch.Failed] with an error value.
func (b *Batch) FailedErr(id string, err error) {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	b.Failed(id, reason)
}

// FailedReason is [Batch.Failed] with a stable classification alongside the
// prose, so the run report can tally the cause instead of the wording.
//
// detail is what Failed would have taken as its reason, and is subject to the
// same placeholder rule — an empty one is never rendered blank.
func (b *Batch) FailedReason(id, code, detail string) {
	detail = strings.TrimSpace(detail)
	normalized := b.reasonCode(id, code)
	if detail == "" {
		detail = "component reported a failure without a reason"
		if normalized == "" {
			normalized = ReasonCodeNoReason
		}
	}
	b.record(SyncResult{
		ID: id, Outcome: OutcomeFailed,
		Error: detail, ReasonCode: normalized, ReasonDetail: detail,
	})
}

// reasonCode validates a component-supplied code, recording a defect (which the
// server logs at ERROR) when it is refused. A refused code is dropped, never
// substituted: see normalizeReasonCode.
func (b *Batch) reasonCode(id, code string) string {
	out, defect := normalizeReasonCode(code)
	if defect != "" {
		b.defects = append(b.defects, fmt.Sprintf("id %q: %s", id, defect))
	}
	return out
}

// Record stores an arbitrary verdict. Prefer the named helpers; this exists for
// a component that computes a SyncResult generically, or replays a set it
// recorded earlier (the batch_id dedup pattern).
//
// Its reason code gets the named helpers' checks: a reserved-prefix, overlong or
// whitespace-bearing code is dropped with a defect, never forwarded. The one
// exception is a failed verdict carrying one of this package's own ReasonCode*
// constants, which [Batch.Results] hands out and a replay must be able to record
// again unchanged.
func (b *Batch) Record(r SyncResult) {
	if !isSDKMintedReplay(r) {
		r.ReasonCode = b.reasonCode(r.ID, r.ReasonCode)
	}
	b.record(r)
}

func (b *Batch) record(r SyncResult) {
	if _, ok := b.known[r.ID]; !ok {
		// Dropped rather than forwarded: the platform rejects the whole batch on
		// an unrequested id, so passing it through would discard every good
		// correlation alongside it.
		b.defects = append(b.defects, fmt.Sprintf(
			"verdict for id %q, which was not in the batch (dropped)", r.ID))
		return
	}
	if _, dup := b.results[r.ID]; dup {
		b.defects = append(b.defects, fmt.Sprintf(
			"duplicate verdict for id %q (last one wins)", r.ID))
	}
	b.results[r.ID] = r
}

// Pending returns the requested ids that have no verdict yet, in request order.
// Useful for a component that pushes in sub-groups and wants to sweep up the
// remainder itself with a specific reason.
func (b *Batch) Pending() []string {
	out := make([]string, 0, len(b.ids)-len(b.results))
	for _, id := range b.ids {
		if _, ok := b.results[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Len reports the number of entities in the batch.
func (b *Batch) Len() int { return len(b.ids) }

// Results returns one verdict per requested id, in request order, alongside the
// component-bug descriptions collected while building it (which the server logs
// at ERROR).
//
// These component bugs are NORMALIZED to a per-entity failure rather than
// propagated:
//
//   - no verdict was recorded for a requested id;
//   - created/updated carried no external record id;
//   - the outcome is not a recognized value, or is one the batch's lane does not
//     accept (push lanes: created, updated, skipped, failed; withdrawal lanes:
//     withdrawn, skipped, failed).
//
// Each of those would otherwise make the platform discard the whole batch, so
// normalizing keeps the other entities' correlations. It is not a silent
// swallow: the entity is reported FAILED (never skipped, which is a success and
// would drop it permanently from the operator's view), the failure reason names
// the defect, and the defect is logged. Do not "fix" this by returning an error
// instead — a rejected reply is retried with the same input, so a deterministic
// component bug would burn the retry budget and still correlate nothing.
func (b *Batch) Results() ([]SyncResult, []string) {
	defects := b.defects
	out := make([]SyncResult, 0, len(b.ids))
	for _, id := range b.ids {
		r, ok := b.results[id]
		if !ok {
			defects = append(defects, fmt.Sprintf("no verdict for requested id %q", id))
			out = append(out, normalizedFailure(id, ReasonCodeNoVerdict,
				"component returned no verdict for this entity"))
			continue
		}
		if f, defect, bad := outcomeViolation(b.verb, r.Outcome, id); bad {
			defects = append(defects, defect)
			out = append(out, f)
			continue
		}
		if (r.Outcome == OutcomeCreated || r.Outcome == OutcomeUpdated) && r.ExternalRecordID == "" {
			defects = append(defects, fmt.Sprintf(
				"outcome %s for id %q carries no external_record_id", r.Outcome, id))
			out = append(out, normalizedFailure(id, ReasonCodeMissingExternalRecordID,
				fmt.Sprintf("component reported %s without an external_record_id", r.Outcome)))
			continue
		}
		out = append(out, r)
	}
	return out, defects
}

// outcomeViolation reports whether outcome o is unacceptable on a lane speaking
// verb, returning the normalized failure and the defect to log when it is.
//
// Shared by both batch types so the entity and relationship lanes cannot drift
// in which outcomes they accept or in how a violation reads.
func outcomeViolation(verb laneVerb, o Outcome, id string) (SyncResult, string, bool) {
	if !o.Valid() {
		return normalizedFailure(id, ReasonCodeUnrecognizedOutcome,
				fmt.Sprintf("component reported unrecognized outcome %q", o)),
			fmt.Sprintf("unrecognized outcome %q for id %q", o, id), true
	}
	if !verb.permits(o) {
		return normalizedFailure(id, ReasonCodeUnrecognizedOutcome,
				fmt.Sprintf("component reported outcome %q, which a %s lane does not accept", o, verb)),
			fmt.Sprintf("outcome %q for id %q is not valid on a %s lane", o, id, verb), true
	}
	return SyncResult{}, "", false
}

// normalizedFailure builds the per-record failure a component bug is turned into.
//
// Shared by both batch types so the two cannot drift in what they report, and so
// the SDK-minted code and the prose that explains it are always assigned together
// — a code with no prose is unreadable, prose with no code is ungroupable.
func normalizedFailure(id, code, detail string) SyncResult {
	return SyncResult{
		ID: id, Outcome: OutcomeFailed,
		Error: detail, ReasonCode: code, ReasonDetail: detail,
	}
}
