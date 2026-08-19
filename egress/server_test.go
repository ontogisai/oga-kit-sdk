package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubComponent implements Component with pluggable behavior.
type stubComponent struct {
	sync    func(ctx context.Context, req *SyncRequest, b *Batch) error
	health  Health
	connect error
	types   []string // non-nil ⇒ also implements EntityTypeLister via listerComponent
}

func (s *stubComponent) Connect(context.Context) error { return s.connect }
func (s *stubComponent) Health(context.Context) Health { return s.health }
func (s *stubComponent) Sync(ctx context.Context, req *SyncRequest, b *Batch) error {
	if s.sync == nil {
		return nil
	}
	return s.sync(ctx, req, b)
}

// listerComponent adds the optional introspection interface.
type listerComponent struct {
	*stubComponent
}

func (l listerComponent) EntityTypes(context.Context) []string { return l.types }

// quietServer builds a server whose logger discards output, so a test that
// deliberately triggers a defect log does not spam the run.
func quietServer(impl Component) *server {
	cfg := &Config{Port: "0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg.defaults()
	return &server{cfg: cfg, impl: impl}
}

func postSync(t *testing.T, impl Component, req SyncRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return postSyncRaw(t, impl, body)
}

func postSyncRaw(t *testing.T, impl Component, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, PathSync, bytes.NewReader(body))
	w := httptest.NewRecorder()
	quietServer(impl).mux().ServeHTTP(w, r)
	return w
}

func decodeResults(t *testing.T, w *httptest.ResponseRecorder) []SyncResult {
	t.Helper()
	var resp SyncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return resp.Results
}

func twoEntityRequest() SyncRequest {
	return SyncRequest{
		TenantID: "sjcs", ExternalSystem: "24k-core", EntityType: "Equipment",
		Mode: ModeBulk, BatchID: "b-1",
		Entities: []Entity{
			{ID: "e1", EntityType: "Equipment"},
			{ID: "e2", EntityType: "Equipment",
				Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-2"}},
		},
	}
}

func TestSync_HappyPath(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, req *SyncRequest, b *Batch) error {
		if b.Len() != 2 {
			t.Errorf("Batch.Len = %d, want 2", b.Len())
		}
		for _, e := range req.Entities {
			if e.Correlation == nil {
				b.Created(e.ID, "CORE-NEW-"+e.ID)
			} else {
				b.Updated(e.ID, e.Correlation.ExternalRecordID)
			}
		}
		return nil
	}}
	w := postSync(t, impl, twoEntityRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	results := decodeResults(t, w)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	// Request order is part of what the platform validates; assert it explicitly.
	if results[0].ID != "e1" || results[0].Outcome != OutcomeCreated || results[0].ExternalRecordID != "CORE-NEW-e1" {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].ID != "e2" || results[1].Outcome != OutcomeUpdated || results[1].ExternalRecordID != "CORE-2" {
		t.Errorf("results[1] = %+v", results[1])
	}
}

// A per-entity failure must NOT cost the other entity its correlation — that is
// the whole reason failures are recorded on the Batch instead of returned.
func TestSync_PerEntityFailureKeepsOtherCorrelations(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Created("e1", "CORE-1")
		b.FailedErr("e2", errors.New("rejected: missing site"))
		return nil
	}}
	results := decodeResults(t, postSync(t, impl, twoEntityRequest()))
	if results[0].Outcome != OutcomeCreated || results[0].ExternalRecordID != "CORE-1" {
		t.Errorf("results[0] = %+v, want a preserved correlation", results[0])
	}
	if results[1].Outcome != OutcomeFailed || !strings.Contains(results[1].Error, "missing site") {
		t.Errorf("results[1] = %+v", results[1])
	}
}

// An entity the component forgot is reported FAILED, not skipped and not
// omitted: omitting it would make the platform discard the whole batch, and
// skipping it is a SUCCESS that would drop the entity permanently.
func TestSync_MissingVerdictBecomesFailureNotSkip(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Created("e1", "CORE-1")
		return nil // e2 forgotten
	}}
	w := postSync(t, impl, twoEntityRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: a component bookkeeping bug must not fail the batch", w.Code)
	}
	results := decodeResults(t, w)
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per requested id", len(results))
	}
	if results[1].ID != "e2" || results[1].Outcome != OutcomeFailed {
		t.Errorf("results[1] = %+v, want e2 failed", results[1])
	}
	if results[1].Outcome == OutcomeSkipped {
		t.Error("a forgotten entity must never be reported as skipped (a success)")
	}
}

// created/updated with no external id is unusable — the platform would reject
// the whole batch — so it is downgraded to a failure for that entity only.
func TestSync_CreatedWithoutExternalIDBecomesFailure(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Created("e1", "")
		b.Skipped("e2")
		return nil
	}}
	results := decodeResults(t, postSync(t, impl, twoEntityRequest()))
	if results[0].Outcome != OutcomeFailed || !strings.Contains(results[0].Error, "external_record_id") {
		t.Errorf("results[0] = %+v, want a failure naming the missing id", results[0])
	}
	if results[1].Outcome != OutcomeSkipped {
		t.Errorf("results[1] = %+v, want the other entity untouched", results[1])
	}
}

func TestSync_UnrecognizedOutcomeBecomesFailure(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Record(SyncResult{ID: "e1", Outcome: Outcome("done")})
		b.Skipped("e2")
		return nil
	}}
	results := decodeResults(t, postSync(t, impl, twoEntityRequest()))
	if results[0].Outcome != OutcomeFailed || !strings.Contains(results[0].Error, "done") {
		t.Errorf("results[0] = %+v, want a failure naming the bad outcome", results[0])
	}
}

// A verdict for an id that was not requested is DROPPED, not forwarded: the
// platform rejects an unrequested id and would discard every good correlation
// alongside it.
func TestSync_UnrequestedIDIsDropped(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Created("e1", "CORE-1")
		b.Created("e2", "CORE-2")
		b.Created("ghost", "CORE-9")
		return nil
	}}
	results := decodeResults(t, postSync(t, impl, twoEntityRequest()))
	if len(results) != 2 {
		t.Fatalf("results = %d, want exactly the 2 requested", len(results))
	}
	for _, r := range results {
		if r.ID == "ghost" {
			t.Error("unrequested id reached the wire; the platform would discard the batch")
		}
	}
}

// A batch-wide fault becomes 500, which the platform classifies as transient and
// retries with the same batch id.
func TestSync_BatchWideErrorIs500(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, _ *Batch) error {
		return errors.New("external system unreachable")
	}}
	w := postSync(t, impl, twoEntityRequest())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unreachable") {
		t.Errorf("body = %q, want the cause", w.Body.String())
	}
}

// Throttling must surface as 429 + Retry-After, the pair the platform's retry
// classifier reads.
func TestSync_ThrottleIs429WithRetryAfter(t *testing.T) {
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, _ *Batch) error {
		return Throttled(errors.New("quota exhausted"), 45*time.Second)
	}}
	w := postSync(t, impl, twoEntityRequest())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "45" {
		t.Errorf("Retry-After = %q, want 45", got)
	}
	if !errors.Is(Throttled(errBoom, time.Second), errBoom) {
		t.Error("ThrottleError must unwrap to its cause")
	}
}

var errBoom = errors.New("boom")

// Homogeneity is enforced so a component may rely on it. A mixed batch is a
// caller bug and 400 is permanent — the platform surfaces it instead of retrying.
func TestSync_MixedEntityTypeRejected(t *testing.T) {
	req := twoEntityRequest()
	req.Entities[1].EntityType = "Location"
	called := false
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, _ *Batch) error {
		called = true
		return nil
	}}
	w := postSync(t, impl, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if called {
		t.Error("Sync was invoked with a non-homogeneous batch")
	}
}

func TestSync_MissingEntityTypeRejected(t *testing.T) {
	req := twoEntityRequest()
	req.EntityType = ""
	if w := postSync(t, impl400(), req); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func impl400() Component {
	return &stubComponent{sync: func(_ context.Context, _ *SyncRequest, _ *Batch) error {
		return errors.New("must not be called")
	}}
}

func TestSync_MalformedBodyIs400(t *testing.T) {
	if w := postSyncRaw(t, impl400(), []byte("{not json")); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// An empty batch is answered with an empty result list rather than an error: the
// platform short-circuits an empty push, so reaching here is harmless.
func TestSync_EmptyBatchReturnsEmptyResults(t *testing.T) {
	req := twoEntityRequest()
	req.Entities = nil
	w := postSync(t, impl400(), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"results":[]}` {
		t.Errorf("body = %q", got)
	}
}

// Forward compatibility: a field a newer platform adds must not break an
// already-deployed component.
func TestSync_UnknownWireFieldsAccepted(t *testing.T) {
	body := []byte(`{"tenant_id":"sjcs","entity_type":"Equipment","mode":"bulk","batch_id":"b-1",
	  "future_knob":"x","entities":[{"id":"e1","entity_type":"Equipment","future_field":7}]}`)
	impl := &stubComponent{sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Skipped("e1")
		return nil
	}}
	w := postSyncRaw(t, impl, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
}

func TestSync_OversizeBodyIs400(t *testing.T) {
	cfg := &Config{Port: "0", MaxRequestBytes: 64, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg.defaults()
	s := &server{cfg: cfg, impl: impl400()}
	big, err := json.Marshal(SyncRequest{
		EntityType: "Equipment",
		Entities:   []Entity{{ID: strings.Repeat("x", 500), EntityType: "Equipment"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.mux().ServeHTTP(w, httptest.NewRequest(http.MethodPost, PathSync, bytes.NewReader(big)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHealthz_ReflectsComponentHealth(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Health
		want int
	}{
		{"healthy", Health{OK: true, Message: "connected"}, http.StatusOK},
		{"unhealthy", Health{OK: false, Message: "auth expired"}, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			quietServer(&stubComponent{health: tc.h}).mux().
				ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathHealthz, nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			var got Health
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != tc.h {
				t.Errorf("body = %+v, want %+v", got, tc.h)
			}
		})
	}
}

func TestEntityTypes_OptionalInterface(t *testing.T) {
	// Not implemented ⇒ 404, never an empty list (which would read as "supports
	// nothing" rather than "does not answer").
	w := httptest.NewRecorder()
	quietServer(&stubComponent{}).mux().
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathEntityTypes, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}

	// Implemented ⇒ the declared list.
	lister := listerComponent{&stubComponent{types: []string{"Equipment", "Location"}}}
	w = httptest.NewRecorder()
	quietServer(lister).mux().
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathEntityTypes, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body EntityTypesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.EntityTypes) != 2 || body.EntityTypes[0] != "Equipment" {
		t.Errorf("entity_types = %v", body.EntityTypes)
	}
}

func TestListenAndServe_Validation(t *testing.T) {
	ctx := t.Context()
	if err := ListenAndServe(ctx, nil, &stubComponent{}); err == nil {
		t.Error("nil config accepted")
	}
	if err := ListenAndServe(ctx, &Config{}, &stubComponent{}); err == nil {
		t.Error("missing port accepted")
	}
	if err := ListenAndServe(ctx, &Config{Port: "0"}, nil); err == nil {
		t.Error("nil impl accepted")
	}
	// A component that cannot reach its external system must fail startup rather
	// than accept batches it would fail one entity at a time.
	err := ListenAndServe(ctx, &Config{Port: "0"}, &stubComponent{connect: errBoom})
	if err == nil || !errors.Is(err, errBoom) {
		t.Errorf("connect failure = %v, want it to abort startup", err)
	}
}

func TestPending(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	b.Skipped("b")
	got := b.Pending()
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("Pending = %v, want [a c] in request order", got)
	}
}

// A repeated id in the request is answered ONCE: the platform rejects a
// duplicate result.
func TestBatch_DuplicateRequestIDAnsweredOnce(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}, {ID: "a"}})
	b.Skipped("a")
	results, _ := b.Results()
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
}

// An entity with no id cannot be answered at all (the platform keys results by
// id), so it is excluded rather than answered with an empty id.
func TestBatch_IDLessEntityExcluded(t *testing.T) {
	b := newBatch([]Entity{{ID: ""}, {ID: "a"}})
	b.Skipped("a")
	results, _ := b.Results()
	if len(results) != 1 || results[0].ID != "a" {
		t.Errorf("results = %+v, want only a", results)
	}
}

func TestBatch_FailedWithoutReasonStillCarriesOne(t *testing.T) {
	b := newBatch([]Entity{{ID: "a"}})
	b.Failed("a", "")
	results, _ := b.Results()
	if results[0].Error == "" {
		t.Error("a failure reached the operator's run report with no reason")
	}
	b2 := newBatch([]Entity{{ID: "a"}})
	b2.FailedErr("a", nil)
	r2, _ := b2.Results()
	if r2[0].Error == "" {
		t.Error("FailedErr(nil) left an empty reason")
	}
}
