package outcomereport

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// A delta egress report as the platform builds it once runs are started by a
// publication: mode, triggers, the publication delivered, the three new dense
// buckets, and a failed record carrying its history.
const deltaEgressReportJSON = `{
  "schema_version": 1,
  "stage": "egress",
  "delivery_id": "egress:tnt1:run-1",
  "tenant_id": "tnt1",
  "kit_id": "example-kit",
  "status": "completed",
  "started_at": "2026-10-20T09:03:10Z",
  "completed_at": "2026-10-20T09:04:02Z",
  "run": {
    "run_id": "run-1",
    "component": "example-egress",
    "mode": "delta",
    "trigger_kinds": ["ingestion", "ontology"],
    "publications": [
      {"kind": "ontology", "ref": "cr-1", "upstream_ref": "2026-10-20T09:00:00Z", "taken_in_at": "2026-10-20T09:01:30Z"},
      {"kind": "data", "ref": "load-1", "upstream_ref": "2026-10-20T09:00:00Z", "taken_in_at": "2026-10-20T09:02:55Z"}
    ],
    "publications_count": 2,
    "publications_truncated": false,
    "publications_cap": 50
  },
  "counts": {"pushed": 47, "created": 3, "updated": 41, "skipped": 2, "failed": 1, "correlated": 44,
             "withdrawn": 5, "outstanding": 1, "needs_attention": 1},
  "skip_reasons": {"stale_version": 2},
  "fail_reasons": {"target_unroutable": 1},
  "failed_records": [
    {"id": "eq-9", "kind": "entity", "reason_code": "target_unroutable", "reason": "no target",
     "first_failed_at": "2026-10-18T02:10:00Z", "attempts": 3, "needs_attention": true}
  ],
  "failed_records_truncated": false,
  "rejected_records_truncated": false,
  "reason_keys_truncated": false
}`

func TestReport_DeltaEgressFieldsDecode(t *testing.T) {
	var rep Report
	if err := json.Unmarshal([]byte(deltaEgressReportJSON), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Run == nil || rep.Run.Mode != RunModeDelta {
		t.Fatalf("run.mode = %+v, want delta", rep.Run)
	}
	if !reflect.DeepEqual(rep.Run.TriggerKinds, []TriggerKind{TriggerIngestion, TriggerOntology}) {
		t.Fatalf("trigger_kinds = %v", rep.Run.TriggerKinds)
	}
	if len(rep.Run.Publications) != 2 || rep.Run.PublicationsCount != 2 || rep.Run.PublicationsCap != 50 {
		t.Fatalf("publications = %+v", rep.Run)
	}
	p := rep.Run.Publications[1]
	if p.Kind != PublicationData || p.Ref != "load-1" || p.UpstreamRef != "2026-10-20T09:00:00Z" || p.TakenInAt == "" {
		t.Fatalf("publication = %+v", p)
	}
	for k, want := range map[string]int{CountWithdrawn: 5, CountOutstanding: 1, CountNeedsAttention: 1, CountPushed: 47} {
		if got, ok := rep.Count(k); !ok || got != want {
			t.Fatalf("count %s = %d,%v want %d", k, got, ok, want)
		}
	}
	fr := rep.FailedRecords[0]
	if fr.FirstFailedAt != "2026-10-18T02:10:00Z" || fr.Attempts != 3 || !fr.NeedsAttention {
		t.Fatalf("failed record history = %+v", fr)
	}
	if rep.Deferral != nil || rep.OntologyWait != "" || rep.OntologyChange != nil {
		t.Fatal("fields of other stages must stay absent")
	}

	// Re-marshal and compare as data: nothing the platform sent is dropped.
	out, err := json.Marshal(&rep)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal([]byte(deltaEgressReportJSON), &a)
	_ = json.Unmarshal(out, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("round trip changed the report:\nsent %v\ngot  %v", a, b)
	}
}

// A deferred report carries no counts and no run id, and the handler accepts it:
// Validate is minimal by design.
func TestReport_DeferredIsAcceptedWithoutCounts(t *testing.T) {
	const raw = `{
  "schema_version": 1,
  "stage": "egress",
  "delivery_id": "egress:tnt1:deferred:example-egress:data:load-1",
  "tenant_id": "tnt1",
  "status": "deferred",
  "deferral": {"reason": "tenant_auto_run_off"},
  "run": {
    "run_id": "",
    "component": "example-egress",
    "publications": [{"kind": "data", "ref": "load-1", "upstream_ref": "v7", "taken_in_at": "2026-10-20T09:02:55Z"}],
    "publications_count": 1,
    "publications_truncated": false
  },
  "failed_records_truncated": false,
  "rejected_records_truncated": false,
  "reason_keys_truncated": false
}`
	var rep Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != StatusDeferred || rep.Deferral == nil || rep.Deferral.Reason != DeferralTenantAutoRunOff {
		t.Fatalf("deferral = %+v / %+v", rep.Status, rep.Deferral)
	}
	if rep.Counts != nil {
		t.Fatalf("a deferred report has no counts, got %v", rep.Counts)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate refused a deferred report: %v", err)
	}
	if rep.IsTerminalFailure() {
		t.Fatal("a deferral is not a failure")
	}

	stub := &stubReceiver{}
	w := post(t, Handler(Config{Receiver: stub, Logger: quietLogger()}), []byte(raw))
	if w.Code != http.StatusNoContent || stub.got == nil || stub.got.Status != StatusDeferred {
		t.Fatalf("handler: status %d, got %+v", w.Code, stub.got)
	}

	// An unknown deferral reason still decodes: the list may grow.
	var unknown Report
	if err := json.Unmarshal([]byte(`{"deferral":{"reason":"maintenance_freeze"}}`), &unknown); err != nil || unknown.Deferral.Reason != "maintenance_freeze" {
		t.Fatalf("unknown reason: %v %+v", err, unknown.Deferral)
	}
}

func TestReport_IngestionOntologyFieldsDecode(t *testing.T) {
	const raw = `{
  "schema_version": 1,
  "stage": "ingestion",
  "delivery_id": "ingestion:tnt1:load-2",
  "tenant_id": "tnt1",
  "status": "completed",
  "submission": {"job_id": "load-2", "kind": "data", "submitted_by": "example-connector", "upstream_ref": "v8"},
  "counts": {"vertices_created": 4, "vertices_removed": 2, "vertices_revived": 1, "edges_closed_by_removal": 3, "records_quarantined": 4},
  "ontology_wait": "bound",
  "ontology_change": {"status": "awaiting_review", "change_request_id": "cr-2", "upstream_version": "v8",
                      "added_types": ["brick:Example_Class"], "added_types_truncated": false, "added_types_cap": 50},
  "quarantined_by_type": {"brick:Example_Class": 4},
  "rejected_records_truncated": false,
  "failed_records_truncated": false,
  "reason_keys_truncated": false
}`
	var rep Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OntologyWait != OntologyWaitBound {
		t.Fatalf("ontology_wait = %q", rep.OntologyWait)
	}
	oc := rep.OntologyChange
	if oc == nil || oc.Status != OntologyChangeAwaitingReview || oc.ChangeRequestID != "cr-2" ||
		oc.UpstreamVersion != "v8" || len(oc.AddedTypes) != 1 || oc.AddedTypesCap != 50 {
		t.Fatalf("ontology_change = %+v", oc)
	}
	for k, want := range map[string]int{CountVerticesRemoved: 2, CountVerticesRevived: 1, CountEdgesClosedByRemoval: 3} {
		if got := rep.CountOrZero(k); got != want {
			t.Fatalf("count %s = %d want %d", k, got, want)
		}
	}
	if rep.Run != nil || rep.Deferral != nil {
		t.Fatal("egress fields must stay absent on an ingestion report")
	}
}

// The always-sent flags are sent even when false, so their absence keeps
// meaning "older platform".
func TestReport_PublicationFlagsAlwaysSent(t *testing.T) {
	rep := Report{Stage: StageEgress, Run: &Run{RunID: "r", Component: "c"}}
	raw, err := json.Marshal(&rep)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Run map[string]any `json:"run"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"publications_count", "publications_truncated"} {
		if _, ok := m.Run[k]; !ok {
			t.Fatalf("run.%s must always be sent, got %s", k, raw)
		}
	}
}
