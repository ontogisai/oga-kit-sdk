package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

// platformVersionedRequestJSON is the request shape with the version stamp
// (kg-egress-sync D11), RECONCILED like platformResolvedRefsRequestJSON: the
// platform holds the same bytes in its internal/egress wire golden test and
// asserts that marshalling its own SyncRequest produces them (OGA-1114 adds that
// side). Keep the two copies identical.
//
// Two entities: one stamped, one the platform could not stamp, whose field is
// OMITTED rather than sent as zero values a component would read as a real, very
// old version.
const platformVersionedRequestJSON = `{
  "tenant_id": "tnt1",
  "external_system": "ext-system",
  "entity_type": "Equipment",
  "mode": "change",
  "batch_id": "tnt1:example-egress-sync:Equipment:change:1:abc",
  "entities": [
    {
      "id": "eq-1",
      "entity_type": "Equipment",
      "properties": {
        "name": "AHU-1"
      },
      "correlation": {
        "external_system": "ext-system",
        "external_record_id": "EXT-77"
      },
      "version": {
        "created_at": "2026-10-17T05:09:39.832Z",
        "version_no": 7
      }
    },
    {
      "id": "eq-2",
      "entity_type": "Equipment"
    }
  ]
}`

func TestSyncRequest_DecodesPlatformVersionWire(t *testing.T) {
	var req SyncRequest
	if err := json.Unmarshal([]byte(platformVersionedRequestJSON), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(req.Entities))
	}
	got := req.Entities[0].Version
	if got == nil || got.VersionNo != 7 || got.CreatedAt.UnixMilli() != ver("2026-10-17T05:09:39.832Z", 7).CreatedAt.UnixMilli() {
		t.Fatalf("version = %+v, want 2026-10-17T05:09:39.832Z#7", got)
	}
	if req.Entities[1].Version != nil {
		t.Fatalf("an omitted stamp must decode to nil, got %+v", req.Entities[1].Version)
	}

	// And the SDK marshals it back byte for byte, so the fixture also pins the
	// SDK's field order and tags.
	out, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != platformVersionedRequestJSON {
		t.Fatalf("re-marshal differs from the platform fixture:\n%s", out)
	}
}

// staleCheckingComponent is the reference shape the EntityVersion doc tells a
// component to follow: a stamp held per external record, a lock per entity, and
// IsStale before every write and every withdrawal.
type staleCheckingComponent struct {
	withdrawalStub
	mu    sync.Mutex
	held  map[string]*EntityVersion // entity id -> stamp the external record holds
	saw   []*EntityVersion
	sawWd []*EntityVersion
}

func (c *staleCheckingComponent) apply(req *SyncRequest, b *Batch, withdraw bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range req.Entities {
		if withdraw {
			c.sawWd = append(c.sawWd, e.Version)
		} else {
			c.saw = append(c.saw, e.Version)
		}
		if stored := c.held[e.ID]; IsStale(e.Version, stored) {
			b.SkippedReason(e.ID, ReasonStaleVersion, StaleVersionDetail(*e.Version, *stored))
			continue
		}
		c.held[e.ID] = e.Version
		if withdraw {
			b.Withdrawn(e.ID)
		} else {
			b.Updated(e.ID, "EXT-"+e.ID)
		}
	}
}

func (c *staleCheckingComponent) Sync(_ context.Context, req *SyncRequest, b *Batch) error {
	c.apply(req, b, false)
	return nil
}

func (c *staleCheckingComponent) WithdrawEntities(_ context.Context, req *SyncRequest, b *Batch) error {
	c.apply(req, b, true)
	return nil
}

// The stamp reaches a component through the SDK server on the push route and
// the entity withdrawal route, and a stale refusal comes back as a skip whose
// reason code is not dropped (stale_version is not in a reserved namespace).
// The relationship lanes carry no stamp by design: an edge has no version.
func TestVersion_RoundTripsThroughBothEntityRoutes(t *testing.T) {
	c := &staleCheckingComponent{held: map[string]*EntityVersion{}}
	newer := ver("2026-10-17T05:09:39.832Z", 7)
	older := ver("2026-10-17T05:09:39.832Z", 6)

	push := func(v *EntityVersion) SyncResult {
		t.Helper()
		req := SyncRequest{
			TenantID: "tnt1", ExternalSystem: "ext-system", EntityType: "Equipment",
			Mode: ModeChange, BatchID: "b",
			Entities: []Entity{{ID: "eq-1", EntityType: "Equipment", Version: v,
				Correlation: &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-eq-1"}}},
		}
		w := postTo(t, c, PathSync, req)
		if w.Code != http.StatusOK {
			t.Fatalf("push status %d: %s", w.Code, w.Body.String())
		}
		res := decodeResults(t, w)
		if len(res) != 1 {
			t.Fatalf("results = %+v", res)
		}
		return res[0]
	}

	if r := push(newer); r.Outcome != OutcomeUpdated {
		t.Fatalf("first push = %+v, want updated", r)
	}
	r := push(older)
	if r.Outcome != OutcomeSkipped || r.ReasonCode != ReasonStaleVersion {
		t.Fatalf("older push = %+v, want skipped/%s", r, ReasonStaleVersion)
	}
	if r.ReasonDetail == "" {
		t.Fatal("a stale skip must name both stamps in its detail")
	}
	if r := push(newer); r.Outcome != OutcomeUpdated {
		t.Fatalf("equal push = %+v, want updated (an equal stamp is written)", r)
	}
	if r := push(nil); r.Outcome != OutcomeUpdated {
		t.Fatalf("unstamped push = %+v, want updated (a missing stamp is written)", r)
	}
	if len(c.saw) != 4 || c.saw[0].Compare(*newer) != 0 || c.saw[1].Compare(*older) != 0 || c.saw[3] != nil {
		t.Fatalf("component saw %+v, want the stamps as sent (and nil for the unstamped push)", c.saw)
	}

	// Withdrawal carries the tombstone's stamp. One older than what the record
	// holds is refused; a newer one retracts.
	c.held["eq-1"] = newer
	wd := func(v *EntityVersion) SyncResult {
		t.Helper()
		req := entityWithdrawalRequest()
		req.Entities = []Entity{{ID: "eq-1", EntityType: "Equipment", Version: v,
			Correlation: &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-eq-1"}}}
		w := postTo(t, c, PathWithdraw, req)
		if w.Code != http.StatusOK {
			t.Fatalf("withdraw status %d: %s", w.Code, w.Body.String())
		}
		res := decodeResults(t, w)
		if len(res) != 1 {
			t.Fatalf("results = %+v", res)
		}
		return res[0]
	}
	if r := wd(older); r.Outcome != OutcomeSkipped || r.ReasonCode != ReasonStaleVersion {
		t.Fatalf("stale withdrawal = %+v, want skipped/%s", r, ReasonStaleVersion)
	}
	tomb := ver("2026-10-17T05:09:39.832Z", 8)
	if r := wd(tomb); r.Outcome != OutcomeWithdrawn {
		t.Fatalf("withdrawal = %+v, want withdrawn", r)
	}
	if len(c.sawWd) != 2 || c.sawWd[1] == nil || c.sawWd[1].Compare(*tomb) != 0 {
		t.Fatalf("withdrawer saw %+v, want the tombstone stamp last", c.sawWd)
	}
}
