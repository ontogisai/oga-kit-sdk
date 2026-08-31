package egress

import (
	"errors"
	"strings"
	"testing"
)

// The reason-carrying skip is ADDITIVE: a component with nothing to explain can
// still report a bare skip, and gets no invented cause.
func TestSkipped_BareFormRecordsNoReason(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.Skipped("a")

	results, defects := b.Results()
	if len(defects) != 0 {
		t.Fatalf("defects = %v, want none", defects)
	}
	r := results[0]
	if r.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped", r.Outcome)
	}
	if r.ReasonCode != "" || r.ReasonDetail != "" {
		t.Errorf("bare skip fabricated a reason: code=%q detail=%q", r.ReasonCode, r.ReasonDetail)
	}
}

func TestSkippedReason_CarriesCodeAndDetail(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.SkippedReason("a", "predicate_unmapped", "  no Core enum mapping for `feeds`  ")

	results, defects := b.Results()
	if len(defects) != 0 {
		t.Fatalf("defects = %v, want none", defects)
	}
	r := results[0]
	if r.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped (a reason must not turn a success into a failure)", r.Outcome)
	}
	if r.ReasonCode != "predicate_unmapped" {
		t.Errorf("reason_code = %q", r.ReasonCode)
	}
	// Trimmed, so a stray newline in a composed message cannot produce a detail
	// that renders as blank-but-present.
	if r.ReasonDetail != "no Core enum mapping for `feeds`" {
		t.Errorf("reason_detail = %q, want trimmed", r.ReasonDetail)
	}
}

// SkippedReason with neither argument is exactly Skipped — no empty-but-present
// field a client could read as an answer.
func TestSkippedReason_EmptyArgumentsDegradeToABareSkip(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.SkippedReason("a", "   ", "\n\t ")

	results, _ := b.Results()
	if r := results[0]; r.ReasonCode != "" || r.ReasonDetail != "" {
		t.Errorf("code=%q detail=%q, want both empty", r.ReasonCode, r.ReasonDetail)
	}
}

// A component may not mint a code in a reserved namespace: that would let its own
// guess read as an authoritative platform or SDK classification in the report.
// The code is DROPPED (never substituted) and the verdict still recorded, so the
// platform reports the record as unattributed — which is true.
func TestReasonCode_ReservedPrefixesAreRefusedNotSubstituted(t *testing.T) {
	for _, code := range []string{"platform:record_vanished", "sdk:no_verdict"} {
		t.Run(code, func(t *testing.T) {
			b := newBatch([]Entity{{ID: "a"}})
			b.SkippedReason("a", code, "detail survives")

			results, defects := b.Results()
			if len(defects) != 1 {
				t.Fatalf("defects = %v, want exactly one naming the refusal", defects)
			}
			if !strings.Contains(defects[0], "reserved") {
				t.Errorf("defect %q does not say the prefix is reserved", defects[0])
			}
			r := results[0]
			if r.ReasonCode != "" {
				t.Errorf("reason_code = %q, want dropped", r.ReasonCode)
			}
			if r.Outcome != OutcomeSkipped {
				t.Errorf("outcome = %q: a refused code must not change the verdict", r.Outcome)
			}
			if r.ReasonDetail != "detail survives" {
				t.Errorf("reason_detail = %q: the prose is not what was refused", r.ReasonDetail)
			}
		})
	}
}

// A code is a grouping KEY, so prose masquerading as one is refused. Both limits
// exist because the platform carries codes in a bounded per-step tally that
// crosses every workflow continuation.
func TestReasonCode_MalformedCodesAreRefused(t *testing.T) {
	cases := map[string]string{
		"whitespace": "no Core enum mapping for this predicate",
		"too long":   strings.Repeat("x", maxReasonCodeLen+1),
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBatch([]Entity{{ID: "a"}})
			b.FailedReason("a", code, "the real detail")

			results, defects := b.Results()
			if len(defects) != 1 {
				t.Fatalf("defects = %v, want exactly one", defects)
			}
			if results[0].ReasonCode != "" {
				t.Errorf("reason_code = %q, want dropped", results[0].ReasonCode)
			}
			if results[0].ReasonDetail != "the real detail" {
				t.Errorf("reason_detail = %q, want preserved", results[0].ReasonDetail)
			}
		})
	}
}

// The legacy Failed keeps writing `error`, so a platform predating the reason
// fields is unaffected — and now also writes the prose to reason_detail so a
// current platform reads one field for both verdict kinds.
func TestFailed_PopulatesLegacyErrorAndReasonDetail(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.Failed("a", "Core rejected the payload")

	results, _ := b.Results()
	r := results[0]
	if r.Error != "Core rejected the payload" {
		t.Errorf("error = %q", r.Error)
	}
	if r.ReasonDetail != "Core rejected the payload" {
		t.Errorf("reason_detail = %q", r.ReasonDetail)
	}
	// No code: this component did not classify the failure, and guessing one from
	// the prose is the grouping-on-prose mistake the code field exists to avoid.
	if r.ReasonCode != "" {
		t.Errorf("reason_code = %q, want empty — Failed classifies nothing", r.ReasonCode)
	}
}

// A failure with no reason at all is the one case Failed CAN classify by itself,
// and it does — the placeholder prose already said this, but prose is not a key.
func TestFailed_ReasonlessFailureGetsAStableCode(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}, {ID: "b"}})
	b.Failed("a", "")
	b.FailedErr("b", nil)

	results, _ := b.Results()
	for _, r := range results {
		if r.ReasonCode != ReasonCodeNoReason {
			t.Errorf("id %q: reason_code = %q, want %q", r.ID, r.ReasonCode, ReasonCodeNoReason)
		}
		if r.ReasonDetail == "" {
			t.Errorf("id %q: reason_detail is blank", r.ID)
		}
	}
}

func TestFailedReason_CarriesCodeAndPreservesLegacyError(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.FailedReason("a", "target_unroutable", "class \"x\" is not routable")

	results, _ := b.Results()
	r := results[0]
	if r.ReasonCode != "target_unroutable" {
		t.Errorf("reason_code = %q", r.ReasonCode)
	}
	if r.Error != r.ReasonDetail || r.Error == "" {
		t.Errorf("error = %q, reason_detail = %q: a failure reports its prose in both", r.Error, r.ReasonDetail)
	}
}

// FailedReason with a code but no detail still gets readable prose — the code
// alone is unreadable to an operator.
func TestFailedReason_EmptyDetailKeepsTheCodeAndGetsPlaceholderProse(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.FailedReason("a", "target_unroutable", "")

	results, _ := b.Results()
	r := results[0]
	if r.ReasonCode != "target_unroutable" {
		t.Errorf("reason_code = %q, want the component's own code, not the no-reason fallback", r.ReasonCode)
	}
	if r.ReasonDetail == "" || r.Error == "" {
		t.Errorf("detail=%q error=%q, want the placeholder rather than blank", r.ReasonDetail, r.Error)
	}
}

// The three component bugs Results() normalizes now arrive with stable codes, so
// a run whose component returned no verdict for 300 records reports ONE cause
// rather than 300 individually-worded failures.
func TestResults_NormalizedComponentBugsCarryStableCodes(t *testing.T) {
	b := newBatch([]Entity{{ID: "silent"}, {ID: "bogus"}, {ID: "idless"}})
	b.Record(SyncResult{ID: "bogus", Outcome: Outcome("nonsense")})
	b.Record(SyncResult{ID: "idless", Outcome: OutcomeCreated}) // no external record id

	results, defects := b.Results()
	if len(defects) != 3 {
		t.Fatalf("defects = %v, want three", defects)
	}
	want := map[string]string{
		"silent": ReasonCodeNoVerdict,
		"bogus":  ReasonCodeUnrecognizedOutcome,
		"idless": ReasonCodeMissingExternalRecordID,
	}
	for _, r := range results {
		if r.Outcome != OutcomeFailed {
			t.Errorf("id %q: outcome = %q, want failed (never skipped — a skip drops it "+
				"permanently from the operator's view)", r.ID, r.Outcome)
		}
		if r.ReasonCode != want[r.ID] {
			t.Errorf("id %q: reason_code = %q, want %q", r.ID, r.ReasonCode, want[r.ID])
		}
		if r.ReasonDetail == "" {
			t.Errorf("id %q: a code with no prose is unreadable", r.ID)
		}
	}
}

// Both batch types must behave identically: the relationship lane is where the
// unmapped-predicate skip actually happens, so a divergence here would leave the
// one call site that motivated this work unattributed.
func TestRelationshipBatch_ReasonBehaviorMatchesEntityBatch(t *testing.T) {
	rb := newRelationshipBatch([]Relationship{{ID: "r1"}, {ID: "r2"}, {ID: "r3"}, {ID: "r4"}})
	rb.Skipped("r1")
	rb.SkippedReason("r2", "predicate_unmapped", "no Core enum mapping")
	rb.FailedReason("r3", "target_unroutable", "class not routable")
	rb.FailedErr("r4", errors.New("boom"))

	results, defects := rb.Results()
	if len(defects) != 0 {
		t.Fatalf("defects = %v, want none", defects)
	}
	byID := map[string]SyncResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if r := byID["r1"]; r.ReasonCode != "" || r.ReasonDetail != "" {
		t.Errorf("r1 bare skip fabricated a reason: %+v", r)
	}
	if r := byID["r2"]; r.Outcome != OutcomeSkipped || r.ReasonCode != "predicate_unmapped" {
		t.Errorf("r2 = %+v", r)
	}
	if r := byID["r3"]; r.Outcome != OutcomeFailed || r.ReasonCode != "target_unroutable" || r.Error == "" {
		t.Errorf("r3 = %+v", r)
	}
	if r := byID["r4"]; r.Error != "boom" || r.ReasonDetail != "boom" {
		t.Errorf("r4 = %+v", r)
	}
}

// A reserved-prefix refusal must be reported on the relationship lane too — the
// two reasonCode helpers are separate methods, so only a test covering both
// catches one of them forgetting to record the defect.
func TestRelationshipBatch_ReservedPrefixRecordsADefect(t *testing.T) {
	rb := newRelationshipBatch([]Relationship{{ID: "r1"}})
	rb.SkippedReason("r1", "platform:record_vanished", "detail")

	results, defects := rb.Results()
	if len(defects) != 1 || !strings.Contains(defects[0], "reserved") {
		t.Fatalf("defects = %v, want one naming the reserved prefix", defects)
	}
	if results[0].ReasonCode != "" {
		t.Errorf("reason_code = %q, want dropped", results[0].ReasonCode)
	}
}
