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

// relationshipStub implements Component AND RelationshipSyncer, recording which
// method the server called.
type relationshipStub struct {
	stubComponent
	relationship func(ctx context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error
	calls        []string
}

// The compile-time assertion this SDK tells kit authors to write.
var _ RelationshipSyncer = (*relationshipStub)(nil)

func (r *relationshipStub) Sync(ctx context.Context, req *SyncRequest, b *Batch) error {
	r.calls = append(r.calls, "Sync")
	return r.stubComponent.Sync(ctx, req, b)
}

func (r *relationshipStub) SyncRelationships(ctx context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error {
	r.calls = append(r.calls, "SyncRelationships")
	if r.relationship == nil {
		return nil
	}
	return r.relationship(ctx, req, b)
}

func postRelationshipSync(t *testing.T, impl Component, req RelationshipSyncRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, PathRelationshipSync, bytes.NewReader(body))
	w := httptest.NewRecorder()
	quietServer(impl).mux().ServeHTTP(w, r)
	return w
}

func twoRelationshipRequest() RelationshipSyncRequest {
	corr := func(id string) *Correlation {
		return &Correlation{ExternalSystem: "24k-core", ExternalRecordID: id}
	}
	return RelationshipSyncRequest{
		TenantID: "sjcs1", ExternalSystem: "24k-core", Predicate: "feeds",
		Mode: ModeBulk, BatchID: "b-1",
		Relationships: []Relationship{
			{
				ID: "rel-1", Predicate: "feeds",
				Source: RelationshipEndpoint{EntityID: "eq-1", EntityType: "Equipment", Correlation: corr("CORE-EQ-1")},
				Target: RelationshipEndpoint{EntityID: "eq-2", EntityType: "Equipment", Correlation: corr("CORE-EQ-2")},
			},
			{
				ID: "rel-2", Predicate: "feeds",
				Source: RelationshipEndpoint{EntityID: "eq-3", EntityType: "Equipment", Correlation: corr("CORE-EQ-3")},
				Target: RelationshipEndpoint{EntityID: "loc-1", EntityType: "rec:HVACZone", Correlation: corr("CORE-SP-1")},
			},
		},
	}
}

// TestRelationshipLane_RouteCallsSyncRelationships is the point of the lane: a
// relationship batch reaches a dedicated handler, never Sync.
func TestRelationshipLane_RouteCallsSyncRelationships(t *testing.T) {
	impl := &relationshipStub{relationship: func(_ context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error {
		for _, rel := range req.Relationships {
			b.Created(rel.ID, "core-rel-"+rel.ID)
		}
		return nil
	}}
	w := postRelationshipSync(t, impl, twoRelationshipRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d; body=%s", PathRelationshipSync, w.Code, w.Body.String())
	}
	if got := strings.Join(impl.calls, ","); got != "SyncRelationships" {
		t.Errorf("called %q, want SyncRelationships", got)
	}
	results := decodeResults(t, w)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if r.Outcome != OutcomeCreated {
			t.Errorf("%s: outcome = %q, want created", r.ID, r.Outcome)
		}
	}
}

// TestRelationshipLane_UnimplementedAnswers501 covers the capability signal —
// same reasoning as the ontology lane's 501: a 404 could not distinguish "this
// component does not serve the lane" from "wrong base URL".
func TestRelationshipLane_UnimplementedAnswers501(t *testing.T) {
	// A plain Component — no RelationshipSyncer. A kit that declares no
	// relationships_sync block is common and legitimate.
	impl := &stubComponent{}

	w := postRelationshipSync(t, impl, twoRelationshipRequest())
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("POST %s = %d, want 501; body=%s", PathRelationshipSync, w.Code, w.Body.String())
	}
	for _, want := range []string{"RelationshipSyncer", "relationships_sync", "SyncRelationships", "var _"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("501 body should mention %q, got: %s", want, w.Body.String())
		}
	}

	t.Run("the entity lane is unaffected", func(t *testing.T) {
		w := postTo(t, impl, PathSync, twoEntityRequest())
		if w.Code != http.StatusOK {
			t.Errorf("POST %s = %d, want 200 — a component without the relationships lane still "+
				"serves entities; body=%s", PathSync, w.Code, w.Body.String())
		}
	})
}

// TestRelationshipLane_501IsDecidedBeforeTheBodyIsRead mirrors the ontology
// lane's equivalent: the capability answer must not depend on the body.
func TestRelationshipLane_501IsDecidedBeforeTheBodyIsRead(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, PathRelationshipSync, strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	quietServer(&stubComponent{}).mux().ServeHTTP(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("POST %s with a malformed body = %d, want 501 (not 400)", PathRelationshipSync, w.Code)
	}
}

// TestRelationshipLane_SharesThePushMachinery asserts the relationships lane's
// serveRelationshipPush behaves identically to servePush's contract, even
// though it cannot literally share the function (different request/batch
// types).
func TestRelationshipLane_SharesThePushMachinery(t *testing.T) {
	req := twoRelationshipRequest()

	t.Run("a throttle becomes 429 with Retry-After", func(t *testing.T) {
		impl := &relationshipStub{relationship: func(_ context.Context, _ *RelationshipSyncRequest, _ *RelationshipBatch) error {
			return Throttled(context.DeadlineExceeded, 5*time.Second)
		}}
		w := postRelationshipSync(t, impl, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("throttled relationship push = %d, want 429", w.Code)
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want \"5\"", got)
		}
	})

	t.Run("a batch-wide fault becomes 500", func(t *testing.T) {
		impl := &relationshipStub{relationship: func(_ context.Context, _ *RelationshipSyncRequest, _ *RelationshipBatch) error {
			return context.DeadlineExceeded
		}}
		if w := postRelationshipSync(t, impl, req); w.Code != http.StatusInternalServerError {
			t.Errorf("failed relationship push = %d, want 500", w.Code)
		}
	})

	t.Run("a malformed verdict is normalized, not forwarded", func(t *testing.T) {
		impl := &relationshipStub{relationship: func(_ context.Context, _ *RelationshipSyncRequest, b *RelationshipBatch) error {
			b.Created("never-requested", "x-1")
			return nil
		}}
		w := postRelationshipSync(t, impl, req)
		if w.Code != http.StatusOK {
			t.Fatalf("= %d, want 200", w.Code)
		}
		results := decodeResults(t, w)
		if len(results) != len(req.Relationships) {
			t.Fatalf("got %d results, want exactly one per requested relationship (%d)",
				len(results), len(req.Relationships))
		}
		for _, r := range results {
			if r.Outcome != OutcomeFailed {
				t.Errorf("%s: outcome = %q, want failed — no verdict was recorded for it", r.ID, r.Outcome)
			}
		}
	})

	t.Run("an empty batch is 200 with no results", func(t *testing.T) {
		empty := req
		empty.Relationships = nil
		impl := &relationshipStub{}
		w := postRelationshipSync(t, impl, empty)
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

	t.Run("a non-homogeneous batch is 400", func(t *testing.T) {
		bad := req
		bad.Relationships = []Relationship{{ID: "rel-1", Predicate: "controls"}}
		if w := postRelationshipSync(t, &relationshipStub{}, bad); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s with a mixed batch = %d, want 400", PathRelationshipSync, w.Code)
		}
	})

	t.Run("a missing predicate is 400", func(t *testing.T) {
		bad := req
		bad.Predicate = ""
		if w := postRelationshipSync(t, &relationshipStub{}, bad); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s with no predicate = %d, want 400", PathRelationshipSync, w.Code)
		}
	})
}

// TestRelationshipLane_SkippedIsASuccess pins Requirement 5.2's behavior at the
// wire level: an unmapped predicate is reported skipped, not failed, and the
// batch is still a clean 200.
func TestRelationshipLane_SkippedIsASuccess(t *testing.T) {
	impl := &relationshipStub{relationship: func(_ context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error {
		for _, rel := range req.Relationships {
			b.Skipped(rel.ID)
		}
		return nil
	}}
	w := postRelationshipSync(t, impl, twoRelationshipRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", w.Code)
	}
	for _, r := range decodeResults(t, w) {
		if r.Outcome != OutcomeSkipped {
			t.Errorf("%s: outcome = %q, want skipped", r.ID, r.Outcome)
		}
	}
}

// TestRelationshipLane_MethodAndPathDiscipline pins the route surface.
func TestRelationshipLane_MethodAndPathDiscipline(t *testing.T) {
	mux := quietServer(&relationshipStub{}).mux()

	t.Run("GET on the relationship path is 405", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathRelationshipSync, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", PathRelationshipSync, w.Code)
		}
	})

	t.Run("the path is the wire literal, distinct from the other two lanes", func(t *testing.T) {
		if PathRelationshipSync != "/egress/relationship-sync" {
			t.Errorf("PathRelationshipSync = %q", PathRelationshipSync)
		}
		if PathRelationshipSync == PathSync || PathRelationshipSync == PathOntologySync {
			t.Error("the relationships lane must not share a path with either other lane — the path IS the record kind")
		}
	})
}

// TestRelationshipEndpoint_CorrelationIsWireLiteral pins that a
// RelationshipEndpoint's Correlation field is NOT omitempty — unlike
// Entity.Correlation, which is legitimately absent for an uncorrelated entity,
// an endpoint's correlation is guaranteed present by the platform (an
// uncorrelated endpoint fails the relationship before it is ever sent), so the
// wire always carries it.
func TestRelationshipEndpoint_CorrelationRoundTrips(t *testing.T) {
	ep := RelationshipEndpoint{
		EntityID: "e-1", EntityType: "Equipment",
		Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-1"},
	}
	raw, err := json.Marshal(ep)
	if err != nil {
		t.Fatal(err)
	}
	var got RelationshipEndpoint
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Correlation == nil || got.Correlation.ExternalRecordID != "CORE-1" {
		t.Errorf("round-tripped correlation = %+v, want ExternalRecordID CORE-1", got.Correlation)
	}
}
