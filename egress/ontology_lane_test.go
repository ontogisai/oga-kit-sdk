package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ontologyStub implements Component AND OntologyTypeSyncer, recording which
// method the server called so a test can assert the ROUTE reached the right one.
type ontologyStub struct {
	stubComponent
	ontology func(ctx context.Context, req *SyncRequest, b *Batch) error
	calls    []string
}

// The compile-time assertion this SDK tells kit authors to write. It is the only
// thing that catches a signature typo, since the server discovers the interface
// with a runtime type assertion.
var _ OntologyTypeSyncer = (*ontologyStub)(nil)

func (o *ontologyStub) Sync(ctx context.Context, req *SyncRequest, b *Batch) error {
	o.calls = append(o.calls, "Sync")
	return o.stubComponent.Sync(ctx, req, b)
}

func (o *ontologyStub) SyncOntologyTypes(ctx context.Context, req *SyncRequest, b *Batch) error {
	o.calls = append(o.calls, "SyncOntologyTypes")
	if o.ontology == nil {
		return nil
	}
	return o.ontology(ctx, req, b)
}

func postTo(t *testing.T, impl Component, path string, req SyncRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	w := httptest.NewRecorder()
	quietServer(impl).mux().ServeHTTP(w, r)
	return w
}

// TestOntologyLane_RouteSelectsTheMethod is the point of the whole split.
//
// The two batches here are BYTE-IDENTICAL — same entity_type, same mode, same
// entities — and differ only in the path they were posted to. Before the split a
// component had to tell them apart from their content, which is exactly the
// ambiguity that could push type records into an asset register.
func TestOntologyLane_RouteSelectsTheMethod(t *testing.T) {
	req := SyncRequest{
		TenantID: "sjcs", ExternalSystem: "24k-core",
		// The colliding label: a kit declares this anchor in BOTH lanes, so the
		// wire label cannot distinguish them.
		EntityType: "Equipment",
		Mode:       ModeBulk,
		BatchID:    "b-1",
		Entities:   []Entity{{ID: "e-1"}},
	}

	t.Run("entity path calls Sync", func(t *testing.T) {
		impl := &ontologyStub{stubComponent: stubComponent{
			sync: func(_ context.Context, _ *SyncRequest, b *Batch) error {
				b.Created("e-1", "core-asset-1")
				return nil
			},
		}}
		w := postTo(t, impl, PathSync, req)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d; body=%s", PathSync, w.Code, w.Body.String())
		}
		if got := strings.Join(impl.calls, ","); got != "Sync" {
			t.Errorf("called %q, want Sync", got)
		}
		if r := decodeResults(t, w)[0]; r.ExternalRecordID != "core-asset-1" {
			t.Errorf("external_record_id = %q, want the entity handler's id", r.ExternalRecordID)
		}
	})

	t.Run("ontology path calls SyncOntologyTypes", func(t *testing.T) {
		impl := &ontologyStub{ontology: func(_ context.Context, _ *SyncRequest, b *Batch) error {
			b.Created("e-1", "core-class-1")
			return nil
		}}
		w := postTo(t, impl, PathOntologySync, req)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d; body=%s", PathOntologySync, w.Code, w.Body.String())
		}
		if got := strings.Join(impl.calls, ","); got != "SyncOntologyTypes" {
			t.Errorf("called %q, want SyncOntologyTypes", got)
		}
		if r := decodeResults(t, w)[0]; r.ExternalRecordID != "core-class-1" {
			t.Errorf("external_record_id = %q, want the ontology handler's id", r.ExternalRecordID)
		}
	})
}

// TestOntologyLane_UnimplementedAnswers501 covers the capability signal.
//
// 501 rather than 404 is load-bearing: a 404 is indistinguishable from a wrong
// base URL, a stale gateway route or a path typo, so it could not tell the
// platform "this component does not serve the lane" apart from "I am talking to
// the wrong thing".
func TestOntologyLane_UnimplementedAnswers501(t *testing.T) {
	// A plain Component — no OntologyTypeSyncer. This is a kit that declares no
	// ontology_sync block, which is a legitimate and common shape.
	impl := &stubComponent{}

	w := postTo(t, impl, PathOntologySync, twoEntityRequest())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("POST %s = %d, want 501; body=%s", PathOntologySync, w.Code, w.Body.String())
	}
	// The message has to carry the fix, because reaching here means the running
	// image is out of step with the manifest that declared the lane.
	for _, want := range []string{"OntologyTypeSyncer", "ontology_sync", "SyncOntologyTypes", "var _"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("501 body should mention %q, got: %s", want, w.Body.String())
		}
	}

	t.Run("the entity lane is unaffected", func(t *testing.T) {
		w := postTo(t, impl, PathSync, twoEntityRequest())
		if w.Code != http.StatusOK {
			t.Errorf("POST %s = %d, want 200 — a component without the ontology lane still serves "+
				"entities; body=%s", PathSync, w.Code, w.Body.String())
		}
	})
}

// TestOntologyLane_501IsDecidedBeforeTheBodyIsRead: the answer cannot depend on
// the body, and a malformed body on a lane this component does not serve should
// report the more fundamental problem rather than a decode error.
func TestOntologyLane_501IsDecidedBeforeTheBodyIsRead(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, PathOntologySync, strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	quietServer(&stubComponent{}).mux().ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("POST %s with a malformed body = %d, want 501 (not 400): the component cannot "+
			"serve this lane whatever the body says", PathOntologySync, w.Code)
	}
}

// TestOntologyLane_SharesThePushMachinery is what keeps the two lanes from
// drifting. Every behavior asserted here is implemented ONCE in servePush, so a
// change to either lane's error handling, verdict normalization or empty-batch
// answer applies to both by construction rather than by discipline.
func TestOntologyLane_SharesThePushMachinery(t *testing.T) {
	req := twoEntityRequest()

	t.Run("a throttle becomes 429 with Retry-After", func(t *testing.T) {
		impl := &ontologyStub{ontology: func(_ context.Context, _ *SyncRequest, _ *Batch) error {
			return Throttled(context.DeadlineExceeded, 7*time.Second)
		}}
		w := postTo(t, impl, PathOntologySync, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("throttled ontology push = %d, want 429", w.Code)
		}
		if got := w.Header().Get("Retry-After"); got != "7" {
			t.Errorf("Retry-After = %q, want \"7\"", got)
		}
	})

	t.Run("a batch-wide fault becomes 500", func(t *testing.T) {
		impl := &ontologyStub{ontology: func(_ context.Context, _ *SyncRequest, _ *Batch) error {
			return context.DeadlineExceeded
		}}
		if w := postTo(t, impl, PathOntologySync, req); w.Code != http.StatusInternalServerError {
			t.Errorf("failed ontology push = %d, want 500", w.Code)
		}
	})

	t.Run("a malformed verdict is normalized, not forwarded", func(t *testing.T) {
		impl := &ontologyStub{ontology: func(_ context.Context, r *SyncRequest, b *Batch) error {
			// A verdict for an id that was never requested: the platform would
			// discard the whole batch, so the SDK normalizes it.
			b.Created("never-requested", "x-1")
			return nil
		}}
		w := postTo(t, impl, PathOntologySync, req)
		if w.Code != http.StatusOK {
			t.Fatalf("= %d, want 200", w.Code)
		}
		results := decodeResults(t, w)
		if len(results) != len(req.Entities) {
			t.Fatalf("got %d results, want exactly one per requested entity (%d)",
				len(results), len(req.Entities))
		}
		for _, r := range results {
			if r.Outcome != OutcomeFailed {
				t.Errorf("%s: outcome = %q, want failed — no verdict was recorded for it", r.ID, r.Outcome)
			}
		}
	})

	t.Run("an empty batch is 200 with no results", func(t *testing.T) {
		empty := req
		empty.Entities = nil
		impl := &ontologyStub{}
		w := postTo(t, impl, PathOntologySync, empty)
		if w.Code != http.StatusOK {
			t.Fatalf("= %d, want 200", w.Code)
		}
		if got := decodeResults(t, w); len(got) != 0 {
			t.Errorf("results = %v, want empty", got)
		}
		if len(impl.calls) != 0 {
			t.Errorf("called %v for an empty batch; the handler should not be invoked", impl.calls)
		}
	})

	t.Run("a non-homogeneous batch is 400 on both lanes", func(t *testing.T) {
		bad := req
		bad.Entities = []Entity{{ID: "e-1", EntityType: "brick:AHU"}}
		for _, path := range []string{PathSync, PathOntologySync} {
			if w := postTo(t, &ontologyStub{}, path, bad); w.Code != http.StatusBadRequest {
				t.Errorf("POST %s with a mixed batch = %d, want 400", path, w.Code)
			}
		}
	})
}

// TestOntologyLane_MethodAndPathDiscipline pins the route surface. A GET on the
// ontology path must not fall through to something else, and the retired
// entity-types path must stay gone.
func TestOntologyLane_MethodAndPathDiscipline(t *testing.T) {
	mux := quietServer(&ontologyStub{}).mux()

	t.Run("GET on the ontology path is 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathOntologySync, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", PathOntologySync, w.Code)
		}
	})

	t.Run("the paths are the wire literals", func(t *testing.T) {
		// Pinned as literals for the same reason PathSync is: a component
		// listening on a renamed path is unreachable, and nothing would fail until
		// a real platform pushed to it.
		if PathOntologySync != "/egress/ontology-sync" {
			t.Errorf("PathOntologySync = %q", PathOntologySync)
		}
		if PathSync == PathOntologySync {
			t.Error("the two lanes must not share a path — the path IS the record kind")
		}
	})
}
