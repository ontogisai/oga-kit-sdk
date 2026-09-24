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

// A missing count key means "this stage does not report that bucket", which is
// a different thing from a bucket that is genuinely zero.
func TestReport_CountDistinguishesAbsentFromZero(t *testing.T) {
	t.Parallel()
	rep := validReport()
	rep.Counts = map[string]int{"edges_created": 0}

	if v, ok := rep.Count("edges_created"); !ok || v != 0 {
		t.Errorf("present-zero: got %d,%v want 0,true", v, ok)
	}
	if v, ok := rep.Count("pushed"); ok || v != 0 {
		t.Errorf("absent: got %d,%v want 0,false", v, ok)
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
