package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withdrawalStub implements Component, both push syncers AND both withdrawers,
// recording which method the server called.
type withdrawalStub struct {
	stubComponent
	withdrawEntities      func(ctx context.Context, req *SyncRequest, b *Batch) error
	withdrawRelationships func(ctx context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error
	calls                 []string
}

// The compile-time assertions this SDK tells kit authors to write.
var (
	_ EntityWithdrawer       = (*withdrawalStub)(nil)
	_ RelationshipWithdrawer = (*withdrawalStub)(nil)
)

func (s *withdrawalStub) Sync(ctx context.Context, req *SyncRequest, b *Batch) error {
	s.calls = append(s.calls, "Sync")
	return s.stubComponent.Sync(ctx, req, b)
}

func (s *withdrawalStub) SyncRelationships(context.Context, *RelationshipSyncRequest, *RelationshipBatch) error {
	s.calls = append(s.calls, "SyncRelationships")
	return nil
}

func (s *withdrawalStub) WithdrawEntities(ctx context.Context, req *SyncRequest, b *Batch) error {
	s.calls = append(s.calls, "WithdrawEntities")
	if s.withdrawEntities == nil {
		return nil
	}
	return s.withdrawEntities(ctx, req, b)
}

func (s *withdrawalStub) WithdrawRelationships(ctx context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error {
	s.calls = append(s.calls, "WithdrawRelationships")
	if s.withdrawRelationships == nil {
		return nil
	}
	return s.withdrawRelationships(ctx, req, b)
}

func postRelationshipTo(t *testing.T, impl Component, path string, req RelationshipSyncRequest) *httptest.ResponseRecorder {
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

// entityWithdrawalRequest is what the platform sends: id, entity type and the
// correlation to retract, no properties, mode change.
func entityWithdrawalRequest() SyncRequest {
	corr := func(id string) *Correlation { return &Correlation{ExternalSystem: "ext-core", ExternalRecordID: id} }
	return SyncRequest{
		TenantID: "tnt1", ExternalSystem: "ext-core", EntityType: "Equipment",
		Mode: ModeChange, BatchID: "w-1",
		Entities: []Entity{
			{ID: "e1", EntityType: "Equipment", Correlation: corr("EXT-1")},
			{ID: "e2", EntityType: "Equipment", Correlation: corr("EXT-2")},
			{ID: "e3", EntityType: "Equipment", Correlation: corr("EXT-3")},
		},
	}
}

// relationshipWithdrawalRequest carries each edge's own correlation, and — unlike
// a push — an endpoint whose correlation the platform no longer holds.
func relationshipWithdrawalRequest() RelationshipSyncRequest {
	corr := func(id string) *Correlation { return &Correlation{ExternalSystem: "ext-core", ExternalRecordID: id} }
	return RelationshipSyncRequest{
		TenantID: "tnt1", ExternalSystem: "ext-core", Predicate: "feeds",
		Mode: ModeChange, BatchID: "wr-1",
		Relationships: []Relationship{
			{
				ID: "rel-1", Predicate: "feeds", Correlation: corr("EXT-REL-1"),
				Source: RelationshipEndpoint{EntityID: "eq-1", EntityType: "Equipment", Correlation: corr("EXT-EQ-1")},
				Target: RelationshipEndpoint{EntityID: "eq-2", EntityType: "Equipment"}, // endpoint already gone
			},
			{
				ID: "rel-2", Predicate: "feeds", Correlation: corr("EXT-REL-2"),
				Source: RelationshipEndpoint{EntityID: "eq-3", EntityType: "Equipment", Correlation: corr("EXT-EQ-3")},
				Target: RelationshipEndpoint{EntityID: "loc-1", EntityType: "rec:HVACZone", Correlation: corr("EXT-SP-1")},
			},
		},
	}
}

func outcomesByID(t *testing.T, w *httptest.ResponseRecorder) map[string]SyncResult {
	t.Helper()
	out := map[string]SyncResult{}
	for _, r := range decodeResults(t, w) {
		out[r.ID] = r
	}
	return out
}

// The paths are the wire contract the platform calls, and each is distinct from
// every other lane's: the path IS the verb.
func TestWithdrawalLanes_PathsAreWireLiterals(t *testing.T) {
	if PathWithdraw != "/egress/withdraw" {
		t.Errorf("PathWithdraw = %q", PathWithdraw)
	}
	if PathRelationshipWithdraw != "/egress/relationship-withdraw" {
		t.Errorf("PathRelationshipWithdraw = %q", PathRelationshipWithdraw)
	}
	seen := map[string]bool{}
	for _, p := range []string{PathSync, PathOntologySync, PathRelationshipSync, PathWithdraw, PathRelationshipWithdraw} {
		if seen[p] {
			t.Errorf("path %q is shared by two lanes", p)
		}
		seen[p] = true
	}
}

// Each withdrawal route reaches its own method, never a push method.
func TestWithdrawalLanes_RouteToTheirOwnMethod(t *testing.T) {
	t.Run("entities", func(t *testing.T) {
		impl := &withdrawalStub{withdrawEntities: func(_ context.Context, req *SyncRequest, b *Batch) error {
			if req.Mode != ModeChange || req.BatchID != "w-1" {
				t.Errorf("request mode/batch_id = %q/%q, want change/w-1", req.Mode, req.BatchID)
			}
			for _, e := range req.Entities {
				if e.Correlation == nil || e.Correlation.ExternalRecordID == "" {
					t.Errorf("%s: no correlation to retract", e.ID)
				}
				b.Withdrawn(e.ID)
			}
			return nil
		}}
		w := postTo(t, impl, PathWithdraw, entityWithdrawalRequest())
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d; body=%s", PathWithdraw, w.Code, w.Body.String())
		}
		if got := strings.Join(impl.calls, ","); got != "WithdrawEntities" {
			t.Errorf("called %q, want WithdrawEntities", got)
		}
		for id, r := range outcomesByID(t, w) {
			if r.Outcome != OutcomeWithdrawn || r.ExternalRecordID != "" {
				t.Errorf("%s: result = %+v, want withdrawn with no external_record_id", id, r)
			}
		}
	})

	t.Run("relationships", func(t *testing.T) {
		impl := &withdrawalStub{withdrawRelationships: func(_ context.Context, req *RelationshipSyncRequest, b *RelationshipBatch) error {
			for _, rel := range req.Relationships {
				if rel.Correlation == nil || rel.Correlation.ExternalRecordID == "" {
					t.Errorf("%s: no correlation to retract", rel.ID)
				}
				b.Withdrawn(rel.ID)
			}
			return nil
		}}
		w := postRelationshipTo(t, impl, PathRelationshipWithdraw, relationshipWithdrawalRequest())
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d; body=%s", PathRelationshipWithdraw, w.Code, w.Body.String())
		}
		if got := strings.Join(impl.calls, ","); got != "WithdrawRelationships" {
			t.Errorf("called %q, want WithdrawRelationships", got)
		}
		for id, r := range outcomesByID(t, w) {
			if r.Outcome != OutcomeWithdrawn {
				t.Errorf("%s: outcome = %q, want withdrawn", id, r.Outcome)
			}
		}
	})
}

// A component that implements neither verb answers 501 on both routes — decided
// before the body is read, and naming what to implement — and is otherwise
// unchanged.
func TestWithdrawalLanes_UnimplementedAnswers501BeforeDecoding(t *testing.T) {
	cases := []struct {
		path, iface, method, flag string
	}{
		{PathWithdraw, "EntityWithdrawer", "WithdrawEntities", "withdrawal.entities"},
		{PathRelationshipWithdraw, "RelationshipWithdrawer", "WithdrawRelationships", "withdrawal.relationships"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{not json"))
			w := httptest.NewRecorder()
			quietServer(&stubComponent{}).mux().ServeHTTP(w, r)
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("POST %s with a malformed body = %d, want 501 (not 400 or 404); body=%s",
					c.path, w.Code, w.Body.String())
			}
			for _, want := range []string{c.iface, c.method, c.flag, "var _"} {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("501 body should mention %q, got: %s", want, w.Body.String())
				}
			}
		})
	}

	t.Run("the push lanes are unaffected", func(t *testing.T) {
		impl := &stubComponent{sync: func(_ context.Context, req *SyncRequest, b *Batch) error {
			for _, e := range req.Entities {
				b.Created(e.ID, "EXT-"+e.ID)
			}
			return nil
		}}
		w := postTo(t, impl, PathSync, twoEntityRequest())
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d; body=%s", PathSync, w.Code, w.Body.String())
		}
		for id, r := range outcomesByID(t, w) {
			if r.Outcome != OutcomeCreated {
				t.Errorf("%s: outcome = %q, want created", id, r.Outcome)
			}
		}
	})
}

// A withdrawal lane accepts withdrawn, skipped and failed, and normalizes a
// push outcome to a per-record failure.
func TestWithdrawalLanes_AcceptOnlyWithdrawalOutcomes(t *testing.T) {
	assertLaneOutcomes := func(t *testing.T, got map[string]SyncResult) {
		t.Helper()
		if r := got["a"]; r.Outcome != OutcomeWithdrawn {
			t.Errorf("withdrawn: got %+v", r)
		}
		if r := got["b"]; r.Outcome != OutcomeSkipped || r.ReasonCode != "already_absent" {
			t.Errorf("skipped: got %+v", r)
		}
		if r := got["c"]; r.Outcome != OutcomeFailed || r.ReasonCode != "has_dependents" {
			t.Errorf("failed: got %+v", r)
		}
		for _, id := range []string{"d", "e"} {
			r := got[id]
			if r.Outcome != OutcomeFailed || r.ReasonCode != ReasonCodeUnrecognizedOutcome {
				t.Errorf("%s: a push outcome on a withdrawal lane = %+v, want failed/%s", id, r, ReasonCodeUnrecognizedOutcome)
			}
			if r.ExternalRecordID != "" {
				t.Errorf("%s: normalized failure forwards external_record_id %q", id, r.ExternalRecordID)
			}
		}
	}
	ids := []string{"a", "b", "c", "d", "e"}

	t.Run("entities", func(t *testing.T) {
		req := entityWithdrawalRequest()
		req.Entities = nil
		for _, id := range ids {
			req.Entities = append(req.Entities, Entity{ID: id, EntityType: "Equipment"})
		}
		impl := &withdrawalStub{withdrawEntities: func(_ context.Context, _ *SyncRequest, b *Batch) error {
			b.Withdrawn("a")
			b.SkippedReason("b", "already_absent", "the record was already gone")
			b.FailedReason("c", "has_dependents", "other records still reference it")
			b.Created("d", "EXT-d")
			b.Updated("e", "EXT-e")
			return nil
		}}
		assertLaneOutcomes(t, outcomesByID(t, postTo(t, impl, PathWithdraw, req)))
	})

	t.Run("relationships", func(t *testing.T) {
		req := relationshipWithdrawalRequest()
		req.Relationships = nil
		for _, id := range ids {
			req.Relationships = append(req.Relationships, Relationship{ID: id, Predicate: "feeds"})
		}
		impl := &withdrawalStub{withdrawRelationships: func(_ context.Context, _ *RelationshipSyncRequest, b *RelationshipBatch) error {
			b.Withdrawn("a")
			b.SkippedReason("b", "already_absent", "the record was already gone")
			b.FailedReason("c", "has_dependents", "other records still reference it")
			b.Created("d", "EXT-d")
			b.Updated("e", "EXT-e")
			return nil
		}}
		assertLaneOutcomes(t, outcomesByID(t, postRelationshipTo(t, impl, PathRelationshipWithdraw, req)))
	})
}

// The other direction: a push lane normalizes `withdrawn` to a per-record
// failure, on all three push lanes.
func TestPushLanes_RejectWithdrawnOutcome(t *testing.T) {
	check := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
		got := outcomesByID(t, w)
		if r := got["e1"]; r.Outcome != OutcomeFailed || r.ReasonCode != ReasonCodeUnrecognizedOutcome {
			t.Errorf("withdrawn on a push lane = %+v, want failed/%s", r, ReasonCodeUnrecognizedOutcome)
		}
		if r := got["e2"]; r.Outcome != OutcomeUpdated {
			t.Errorf("the other record was affected: %+v", r)
		}
	}
	entityVerdicts := func(_ context.Context, _ *SyncRequest, b *Batch) error {
		b.Withdrawn("e1")
		b.Updated("e2", "EXT-2")
		return nil
	}

	t.Run("entities", func(t *testing.T) {
		check(t, postTo(t, &stubComponent{sync: entityVerdicts}, PathSync, twoEntityRequest()))
	})

	t.Run("ontology types", func(t *testing.T) {
		impl := &ontologyStub{ontology: entityVerdicts}
		check(t, postTo(t, impl, PathOntologySync, twoEntityRequest()))
	})

	t.Run("relationships", func(t *testing.T) {
		req := twoRelationshipRequest()
		req.Relationships[0].ID, req.Relationships[1].ID = "e1", "e2"
		impl := &relationshipStub{relationship: func(_ context.Context, _ *RelationshipSyncRequest, b *RelationshipBatch) error {
			b.Withdrawn("e1")
			b.Updated("e2", "EXT-2")
			return nil
		}}
		check(t, postRelationshipSync(t, impl, req))
	})
}

// The withdrawal lanes share the push lanes' batch-wide handling, because they
// delegate to the same serve functions.
func TestWithdrawalLanes_SharePushMachinery(t *testing.T) {
	throttle := Throttled(errors.New("external system rate limit"), 7*time.Second)
	fault := errors.New("external system unreachable")

	t.Run("entity throttle is 429 with Retry-After", func(t *testing.T) {
		impl := &withdrawalStub{withdrawEntities: func(context.Context, *SyncRequest, *Batch) error { return throttle }}
		w := postTo(t, impl, PathWithdraw, entityWithdrawalRequest())
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "7" {
			t.Errorf("= %d Retry-After %q, want 429 / 7", w.Code, w.Header().Get("Retry-After"))
		}
	})

	t.Run("relationship throttle is 429 with Retry-After", func(t *testing.T) {
		impl := &withdrawalStub{withdrawRelationships: func(context.Context, *RelationshipSyncRequest, *RelationshipBatch) error {
			return throttle
		}}
		w := postRelationshipTo(t, impl, PathRelationshipWithdraw, relationshipWithdrawalRequest())
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "7" {
			t.Errorf("= %d Retry-After %q, want 429 / 7", w.Code, w.Header().Get("Retry-After"))
		}
	})

	t.Run("a batch-wide fault is 500 on both", func(t *testing.T) {
		impl := &withdrawalStub{
			withdrawEntities:      func(context.Context, *SyncRequest, *Batch) error { return fault },
			withdrawRelationships: func(context.Context, *RelationshipSyncRequest, *RelationshipBatch) error { return fault },
		}
		if w := postTo(t, impl, PathWithdraw, entityWithdrawalRequest()); w.Code != http.StatusInternalServerError {
			t.Errorf("entity withdrawal fault = %d, want 500", w.Code)
		}
		if w := postRelationshipTo(t, impl, PathRelationshipWithdraw, relationshipWithdrawalRequest()); w.Code != http.StatusInternalServerError {
			t.Errorf("relationship withdrawal fault = %d, want 500", w.Code)
		}
	})

	t.Run("a missing verdict is normalized, not forwarded", func(t *testing.T) {
		impl := &withdrawalStub{withdrawEntities: func(_ context.Context, _ *SyncRequest, b *Batch) error {
			b.Withdrawn("e1")
			return nil
		}}
		got := outcomesByID(t, postTo(t, impl, PathWithdraw, entityWithdrawalRequest()))
		if len(got) != 3 {
			t.Fatalf("got %d results, want one per requested entity", len(got))
		}
		for _, id := range []string{"e2", "e3"} {
			if r := got[id]; r.Outcome != OutcomeFailed || r.ReasonCode != ReasonCodeNoVerdict {
				t.Errorf("%s: %+v, want failed/%s", id, r, ReasonCodeNoVerdict)
			}
		}
	})

	t.Run("gated 503 while the initial Connect is pending", func(t *testing.T) {
		impl := &withdrawalStub{}
		s := quietServer(impl)
		s.connectPending.Store(true)
		for _, p := range []string{PathWithdraw, PathRelationshipWithdraw} {
			w := httptest.NewRecorder()
			s.mux().ServeHTTP(w, httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}")))
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("POST %s while connecting = %d, want 503", p, w.Code)
			}
		}
		if len(impl.calls) != 0 {
			t.Errorf("component called while Connect pending: %v", impl.calls)
		}
	})

	t.Run("a non-homogeneous entity batch is 400", func(t *testing.T) {
		bad := entityWithdrawalRequest()
		bad.Entities[1].EntityType = "Location"
		if w := postTo(t, &withdrawalStub{}, PathWithdraw, bad); w.Code != http.StatusBadRequest {
			t.Errorf("= %d, want 400", w.Code)
		}
	})

	t.Run("a missing relationship verdict is normalized, not forwarded", func(t *testing.T) {
		impl := &withdrawalStub{withdrawRelationships: func(_ context.Context, _ *RelationshipSyncRequest, b *RelationshipBatch) error {
			b.Withdrawn("rel-1")
			return nil
		}}
		got := outcomesByID(t, postRelationshipTo(t, impl, PathRelationshipWithdraw, relationshipWithdrawalRequest()))
		if len(got) != 2 {
			t.Fatalf("got %d results, want one per requested relationship", len(got))
		}
		if r := got["rel-2"]; r.Outcome != OutcomeFailed || r.ReasonCode != ReasonCodeNoVerdict {
			t.Errorf("rel-2: %+v, want failed/%s", r, ReasonCodeNoVerdict)
		}
	})

	t.Run("a non-homogeneous relationship batch is 400", func(t *testing.T) {
		bad := relationshipWithdrawalRequest()
		bad.Relationships[1].Predicate = "hasPoint"
		impl := &withdrawalStub{}
		if w := postRelationshipTo(t, impl, PathRelationshipWithdraw, bad); w.Code != http.StatusBadRequest {
			t.Errorf("= %d, want 400", w.Code)
		}
		if len(impl.calls) != 0 {
			t.Errorf("component called on a rejected batch: %v", impl.calls)
		}
	})
}

// The outcome literal crosses the wire as a string, like the other four.
func TestOutcomeWithdrawn_IsWireLiteral(t *testing.T) {
	if string(OutcomeWithdrawn) != "withdrawn" {
		t.Errorf("OutcomeWithdrawn = %q", OutcomeWithdrawn)
	}
	if !OutcomeWithdrawn.Valid() {
		t.Error("withdrawn reports invalid")
	}
	raw, err := json.Marshal(SyncResult{ID: "a", Outcome: OutcomeWithdrawn})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"id":"a","outcome":"withdrawn"}`; got != want {
		t.Errorf("withdrawn verdict wire = %s, want %s", got, want)
	}
}

// laneVerb.permits is the single table both batch types consult.
func TestLaneVerb_PermitsExactlyItsOutcomes(t *testing.T) {
	want := map[laneVerb]map[Outcome]bool{
		verbPush: {
			OutcomeCreated: true, OutcomeUpdated: true, OutcomeSkipped: true, OutcomeFailed: true,
			OutcomeWithdrawn: false, Outcome("done"): false,
		},
		verbWithdraw: {
			OutcomeCreated: false, OutcomeUpdated: false, OutcomeSkipped: true, OutcomeFailed: true,
			OutcomeWithdrawn: true, Outcome("done"): false,
		},
	}
	for verb, outcomes := range want {
		for o, ok := range outcomes {
			if got := verb.permits(o); got != ok {
				t.Errorf("%s lane permits(%q) = %v, want %v", verb, o, got, ok)
			}
		}
	}
	if verbPush != laneVerb(0) {
		t.Error("the zero laneVerb must be push, so a batch built without one is unchanged")
	}
}
