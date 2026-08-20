package egress

import "fmt"

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
}

// newBatch builds a Batch for the entities of one request.
func newBatch(entities []Entity) *Batch {
	b := &Batch{
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

// Skipped records that id needs no external record, or is already current.
//
// This is a SUCCESS, and it is the right verdict for "nothing to do" — a run
// full of skips is a clean run. Do NOT use it to swallow a problem: a skipped
// entity is never correlated and never retried, so an error reported as a skip
// disappears permanently from the operator's view.
func (b *Batch) Skipped(id string) {
	b.record(SyncResult{ID: id, Outcome: OutcomeSkipped})
}

// Failed records a per-entity failure for id. reason reaches the operator's run
// report, so make it actionable; an empty reason is replaced with a placeholder
// rather than left blank.
//
// This fails THIS entity only — the rest of the batch is still correlated. For a
// batch-wide fault return an error from Sync instead, which lets the platform
// retry the whole batch.
func (b *Batch) Failed(id, reason string) {
	if reason == "" {
		reason = "component reported a failure without a reason"
	}
	b.record(SyncResult{ID: id, Outcome: OutcomeFailed, Error: reason})
}

// FailedErr is [Batch.Failed] with an error value.
func (b *Batch) FailedErr(id string, err error) {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	b.Failed(id, reason)
}

// Record stores an arbitrary verdict. Prefer the named helpers; this exists for
// a component that computes a SyncResult generically.
func (b *Batch) Record(r SyncResult) { b.record(r) }

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
// Three component bugs are NORMALIZED to a per-entity failure rather than
// propagated:
//
//   - no verdict was recorded for a requested id;
//   - created/updated carried no external record id;
//   - the outcome was not one of the four recognized values.
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
			out = append(out, SyncResult{
				ID: id, Outcome: OutcomeFailed,
				Error: "component returned no verdict for this entity",
			})
			continue
		}
		if !r.Outcome.Valid() {
			defects = append(defects, fmt.Sprintf(
				"unrecognized outcome %q for id %q", r.Outcome, id))
			out = append(out, SyncResult{
				ID: id, Outcome: OutcomeFailed,
				Error: fmt.Sprintf("component reported unrecognized outcome %q", r.Outcome),
			})
			continue
		}
		if (r.Outcome == OutcomeCreated || r.Outcome == OutcomeUpdated) && r.ExternalRecordID == "" {
			defects = append(defects, fmt.Sprintf(
				"outcome %s for id %q carries no external_record_id", r.Outcome, id))
			out = append(out, SyncResult{
				ID: id, Outcome: OutcomeFailed,
				Error: fmt.Sprintf("component reported %s without an external_record_id", r.Outcome),
			})
			continue
		}
		out = append(out, r)
	}
	return out, defects
}
