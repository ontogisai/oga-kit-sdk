package outcomereport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// quietLogger keeps the expected-error paths from spamming test output. The
// handler logs rejections at ERROR by design, so several tests below trip it.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type stubReceiver struct {
	got  *Report
	err  error
	call int
}

func (s *stubReceiver) ReceiveOutcomeReport(_ context.Context, rep *Report) error {
	s.call++
	s.got = rep
	return s.err
}

// Pin the interface the way the doc comment tells kit authors to.
var _ Receiver = (*stubReceiver)(nil)

func validReport() *Report {
	return &Report{
		SchemaVersion: SchemaVersion,
		Stage:         StageIngestion,
		DeliveryID:    "ingestion:tenant-a:load-1",
		TenantID:      "tenant-a",
		Status:        StatusCompleted,
		Submission:    &Submission{JobID: "load-1", Kind: "data", SubmittedBy: "a-connector"},
	}
}

func post(t *testing.T, h http.HandlerFunc, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func postReport(t *testing.T, h http.HandlerFunc, rep *Report) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return post(t, h, raw)
}

// A nil Receiver MUST answer 501, not 404 and not a silent 200.
//
// This is the load-bearing case: the platform reads 501 as "does not receive"
// and dead-letters immediately. A 404 would be indistinguishable from a
// misdeployed sidecar, and a 200 would make the platform believe a report was
// accepted that nothing ever read.
func TestHandler_NilReceiverIs501(t *testing.T) {
	t.Parallel()
	h := Handler(Config{Logger: quietLogger()})

	rec := postReport(t, h, validReport())

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
	// The response must name the interface and the compile-time pin, so an
	// operator reading a dead-letter knows what the kit is missing.
	body := rec.Body.String()
	for _, want := range []string{"outcomereport.Receiver", "ReceiveOutcomeReport", "receives_outcome_report"} {
		if !strings.Contains(body, want) {
			t.Errorf("501 body missing %q:\n%s", want, body)
		}
	}
}

func TestHandler_AcceptsAndPassesReportThrough(t *testing.T) {
	t.Parallel()
	rcv := &stubReceiver{}
	h := Handler(Config{Receiver: rcv, Logger: quietLogger()})

	in := validReport()
	in.Counts = map[string]int{"vertices_created": 7}
	in.Submission.UpstreamRef = "2026-09-07T10:15:00Z"

	rec := postReport(t, h, in)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if rcv.call != 1 {
		t.Fatalf("receiver called %d times, want 1", rcv.call)
	}
	if rcv.got.DeliveryID != in.DeliveryID || rcv.got.TenantID != in.TenantID {
		t.Errorf("identity not passed through: %+v", rcv.got)
	}
	if got, ok := rcv.got.Count("vertices_created"); !ok || got != 7 {
		t.Errorf("Count(vertices_created) = %d,%v want 7,true", got, ok)
	}
	// upstream_ref must survive the round trip byte-for-byte — it is the whole
	// point of the field (the platform never parses it).
	if rcv.got.Submission == nil || rcv.got.Submission.UpstreamRef != "2026-09-07T10:15:00Z" {
		t.Errorf("upstream_ref lost: %+v", rcv.got.Submission)
	}
}

// A receiver error is 500 so the platform RETRIES. A receiver that wants a
// report dropped returns nil instead.
func TestHandler_ReceiverErrorIs500(t *testing.T) {
	t.Parallel()
	h := Handler(Config{Receiver: &stubReceiver{err: errors.New("store down")}, Logger: quietLogger()})

	rec := postReport(t, h, validReport())

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// Malformed and unroutable bodies are 400 — retrying them unchanged cannot help.
func TestHandler_BadRequests(t *testing.T) {
	t.Parallel()
	cases := map[string]func() []byte{
		"malformed JSON": func() []byte { return []byte(`{"stage":`) },
		"no delivery_id": func() []byte {
			r := validReport()
			r.DeliveryID = ""
			b, _ := json.Marshal(r)
			return b
		},
		"no tenant_id": func() []byte {
			r := validReport()
			r.TenantID = ""
			b, _ := json.Marshal(r)
			return b
		},
		"no stage": func() []byte {
			r := validReport()
			r.Stage = ""
			b, _ := json.Marshal(r)
			return b
		},
		"no status": func() []byte {
			r := validReport()
			r.Status = ""
			b, _ := json.Marshal(r)
			return b
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rcv := &stubReceiver{}
			h := Handler(Config{Receiver: rcv, Logger: quietLogger()})

			rec := post(t, h, mk())

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if rcv.call != 0 {
				t.Error("receiver was called for an invalid report")
			}
		})
	}
}

// An UNKNOWN stage or status must be accepted, not refused.
//
// The envelope is additive and the platform may add a lane or a terminal state
// (cancelled is already declared and not yet produced). Refusing one would turn
// a forward-compatible platform change into a delivery outage for every
// receiver built against an older SDK.
func TestHandler_UnknownStageAndStatusAreAccepted(t *testing.T) {
	t.Parallel()
	rcv := &stubReceiver{}
	h := Handler(Config{Receiver: rcv, Logger: quietLogger()})

	in := validReport()
	in.Stage = "some_future_lane"
	in.Status = "some_future_state"
	in.SchemaVersion = SchemaVersion + 1

	rec := postReport(t, h, in)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d — forward compatibility broken", rec.Code, http.StatusNoContent)
	}
	if rcv.call != 1 {
		t.Error("receiver not called for a forward-compatible report")
	}
}

// An oversized body is refused rather than read into memory.
func TestHandler_BodyCapIsEnforced(t *testing.T) {
	t.Parallel()
	rcv := &stubReceiver{}
	h := Handler(Config{Receiver: rcv, MaxRequestBytes: 64, Logger: quietLogger()})

	in := validReport()
	in.Warnings = []string{strings.Repeat("x", 512)}

	rec := postReport(t, h, in)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if rcv.call != 0 {
		t.Error("receiver was called for an over-cap body")
	}
}

// The egress shape must decode through the same envelope as the ingestion one —
// one schema, two stages, which is the contract a kit parses against.
func TestReport_EgressShapeDecodes(t *testing.T) {
	t.Parallel()
	const raw = `{
	  "schema_version": 1, "stage": "egress",
	  "delivery_id": "egress:tenant-a:run-9", "tenant_id": "tenant-a",
	  "status": "completed",
	  "run": {"run_id": "run-9", "component": "component-a"},
	  "counts": {"pushed": 10, "failed": 1},
	  "skip_reasons": {"predicate_unmapped": 3},
	  "fail_reasons": {"target_unroutable": 1},
	  "failed_records": [{"id":"e1","kind":"relationship","reason_code":"target_unroutable","reason":"no target"}],
	  "failed_records_truncated": false, "failed_records_cap": 50,
	  "reason_keys_truncated": false, "reason_keys_cap": 32
	}`

	var rep Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Stage != StageEgress || rep.Run == nil || rep.Run.RunID != "run-9" {
		t.Fatalf("egress identity wrong: %+v", rep)
	}
	if rep.Submission != nil {
		t.Error("Submission must be absent on an egress report")
	}
	// Reason tallies must sum to their counts — the reconcilable-reporting
	// property the platform guarantees.
	if got := rep.FailReasons["target_unroutable"]; got != rep.Counts["failed"] {
		t.Errorf("fail_reasons sum = %d, counts.failed = %d", got, rep.Counts["failed"])
	}
	if len(rep.FailedRecords) != 1 || rep.FailedRecords[0].Kind != RecordKindRelationship {
		t.Errorf("failed_records wrong: %+v", rep.FailedRecords)
	}
}

// A completed report carrying rejected records is NOT a failure — the
// distinction is not derivable from the counts, so IsTerminalFailure must key
// on status alone.
func TestReport_IsTerminalFailure(t *testing.T) {
	t.Parallel()
	completedWithRejects := validReport()
	completedWithRejects.RejectedRecords = []RejectedRecord{{
		RecordID: "r1", Kind: RecordKindEntity,
		Disposition: DispositionQuarantined, ErrorCode: "OGA-INGS-VAL-1003",
	}}
	if completedWithRejects.IsTerminalFailure() {
		t.Error("a completed report with rejected records must not read as a terminal failure")
	}

	failed := validReport()
	failed.Status = StatusFailed
	failed.Failure = &Failure{Code: "OGA-EGRS-DEPS-4001", Message: "unreachable"}
	if !failed.IsTerminalFailure() {
		t.Error("a failed report must read as a terminal failure")
	}
}

// Count reports PRESENCE, and presence carries no meaning on the ingestion lane.
//
// This replaces TestReport_CountDistinguishesAbsentFromZero, which asserted the
// opposite. That test passed only because it hand-built `Counts` with an EXPLICIT
// zero — a shape the platform's ingestion projection never produces, since it
// deletes zero buckets. The function was right and the claim about what absence
// MEANS was wrong, so a green test was giving false confidence about the contract.
func TestReport_CountPresenceIsNotSemantic(t *testing.T) {
	t.Parallel()
	// The ingestion lane as the platform actually projects it: SPARSE. A data
	// dispatch that created 7 vertices and changed no edges emits neither
	// edges_updated (zero, omitted) nor types_registered (not measured at all).
	rep := validReport()
	rep.Counts = map[string]int{"vertices_created": 7}

	if v, ok := rep.Count("vertices_created"); !ok || v != 7 {
		t.Errorf("present: got %d,%v want 7,true", v, ok)
	}
	zeroed, zeroedOK := rep.Count("edges_updated")
	notMeasured, notMeasuredOK := rep.Count("types_registered")
	if zeroedOK != notMeasuredOK || zeroed != notMeasured {
		t.Errorf("a zero bucket (%d,%v) and a not-measured bucket (%d,%v) must be "+
			"indistinguishable on the ingestion lane — if they ever differ, the doc "+
			"on Count is wrong again",
			zeroed, zeroedOK, notMeasured, notMeasuredOK)
	}

	// CountOrZero is the accessor for arithmetic, precisely because of the above.
	if got := rep.CountOrZero("edges_updated"); got != 0 {
		t.Errorf("CountOrZero on an absent key = %d, want 0", got)
	}
	if got := rep.CountOrZero("vertices_created"); got != 7 {
		t.Errorf("CountOrZero on a present key = %d, want 7", got)
	}
	// Nil-safe: a failed report carries no counts at all.
	var empty Report
	if got := empty.CountOrZero("pushed"); got != 0 {
		t.Errorf("CountOrZero on a nil map = %d, want 0", got)
	}
	if _, ok := empty.Count("pushed"); ok {
		t.Error("Count on a nil map reported presence")
	}
}

// The egress lane IS dense, so presence there is uninformative for the opposite
// reason: every documented bucket is always sent, including zero. Pinned so a
// future producer change that starts omitting them is caught by a test rather
// than by a receiver computing a wrong total.
func TestReport_EgressCountsAreDense(t *testing.T) {
	t.Parallel()
	rep := validReport()
	rep.Stage = StageEgress
	rep.Counts = map[string]int{
		"pushed": 0, "created": 0, "updated": 0,
		"skipped": 0, "failed": 0, "correlated": 0,
	}
	for _, k := range []string{"pushed", "created", "updated", "skipped", "failed", "correlated"} {
		if _, ok := rep.Count(k); !ok {
			t.Errorf("egress bucket %q absent; the egress lane emits all six, including zero", k)
		}
	}
}

// The two ingestion quarantine breakdowns are keyed in DIFFERENT spaces — entity
// type vs error code — so a receiver must be able to read both without merging
// them. A submission can also fail entirely at the edge stage, in which case the
// vertex breakdown is empty and only the edge one explains the outcome.
func TestReport_BothQuarantineBreakdownsDecode(t *testing.T) {
	t.Parallel()
	const body = `{
	  "schema_version": 1, "stage": "ingestion", "delivery_id": "ingestion:t:j",
	  "tenant_id": "t", "status": "completed",
	  "counts": {"records_quarantined": 5, "edges_quarantined": 3},
	  "quarantined_by_type": {"ex:SomeType": 2},
	  "quarantined_edges_by_reason": {"OGA-INGS-REL-1320": 2, "OGA-INGS-REL-1321": 1}
	}`
	var rep Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rep.QuarantinedByType["ex:SomeType"] != 2 {
		t.Errorf("vertex breakdown: %+v", rep.QuarantinedByType)
	}
	if rep.QuarantinedEdgesByReason["OGA-INGS-REL-1320"] != 2 ||
		rep.QuarantinedEdgesByReason["OGA-INGS-REL-1321"] != 1 {
		t.Errorf("edge breakdown: %+v", rep.QuarantinedEdgesByReason)
	}
	// Reconcilable: the two stages' tallies sum to records_quarantined, and the
	// edge stage's own total is also reported directly.
	vertexTotal, edgeTotal := 0, 0
	for _, v := range rep.QuarantinedByType {
		vertexTotal += v
	}
	for _, v := range rep.QuarantinedEdgesByReason {
		edgeTotal += v
	}
	if got := vertexTotal + edgeTotal; got != rep.CountOrZero("records_quarantined") {
		t.Errorf("tallies sum to %d, want records_quarantined = %d",
			got, rep.CountOrZero("records_quarantined"))
	}
	if edgeTotal != rep.CountOrZero("edges_quarantined") {
		t.Errorf("edge tally sums to %d, want edges_quarantined = %d",
			edgeTotal, rep.CountOrZero("edges_quarantined"))
	}
}

func TestReport_StringNamesTheUnit(t *testing.T) {
	t.Parallel()
	ing := validReport().String()
	if !strings.Contains(ing, "job=load-1") {
		t.Errorf("ingestion summary missing the job: %s", ing)
	}
	eg := &Report{Stage: StageEgress, Status: StatusCompleted, TenantID: "t", DeliveryID: "d",
		Run: &Run{RunID: "run-9"}}
	if !strings.Contains(eg.String(), "run=run-9") {
		t.Errorf("egress summary missing the run: %s", eg.String())
	}
}
