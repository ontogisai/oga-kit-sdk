package egress

import (
	"encoding/json"
	"testing"
)

// parent_refs is the resolved owner of each pushed entity, keyed by the edge name
// the kit declared under entities_sync[].parent_edges.
//
// It is the only way a component can populate an external foreign key: the entity
// as read carries no containment, because containment is an edge and an entity
// read projects columns. The manifest half of this pairing is asserted in
// manifest.TestValidateEgressSyncs_RejectsDuplicateParentEdge and
// _RejectsUnaddressableParentEdges — if the two disagree, a component waits on a
// key that never arrives.
//
// NOTE: the platform-generated golden fixture this file once said was missing now
// exists — platformResolvedRefsRequestJSON in wire_golden_test.go, marshalled from
// the platform's own internal/egress.SyncRequest once it carried parent_refs
// (OGA-822) and type_ancestry (OGA-865). The hand-written body below is retained
// because it asserts the SEMANTICS (which key an owner arrives under, and that an
// absent entry means root) rather than the bytes; the golden fixture is what pins
// the tags and the field order.
func TestParentRefs_DecodesAndKeysOnTheDeclaredEdge(t *testing.T) {
	const body = `{
  "tenant_id": "sjcs",
  "external_system": "24k-core",
  "entity_type": "rec:Room",
  "mode": "bulk",
  "batch_id": "sjcs:core-sync:rec:Room:bulk:2:abc",
  "entities": [
    {
      "id": "019e38e3-room",
      "entity_type": "rec:Room",
      "properties": {"rdfs_label": "SAN 36A-L4-312"},
      "parent_refs": {
        "hasLocation": {
          "entity_id": "019e38e3-level",
          "external_record_id": "8f2c1d04-core-uuid"
        }
      }
    },
    {
      "id": "019e38e3-campus",
      "entity_type": "rec:Building"
    }
  ]
}`
	var req SyncRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(req.Entities))
	}

	ref, ok := req.Entities[0].ParentRefs["hasLocation"]
	if !ok {
		t.Fatalf("parent_refs is keyed on the declared edge name; got keys %v",
			keysOf(req.Entities[0].ParentRefs))
	}
	// The external id is what a foreign key needs; the entity id is for logging.
	if ref.ExternalRecordID != "8f2c1d04-core-uuid" {
		t.Errorf("external_record_id = %q, want the owner's external id", ref.ExternalRecordID)
	}
	if ref.EntityID != "019e38e3-level" {
		t.Errorf("entity_id = %q, want the owner's platform id", ref.EntityID)
	}

	// An ABSENT parent_refs means the entity is a root of that relation. It must
	// decode to an empty map and NOT be mistaken for an error — a campus root has
	// no parent, and omitting the foreign key is the correct outcome.
	if len(req.Entities[1].ParentRefs) != 0 {
		t.Errorf("a root entity carried parent_refs = %v, want none", req.Entities[1].ParentRefs)
	}
}

// An entity with neither parent_refs nor type_ancestry must marshal WITHOUT either
// key, so the older golden fixtures (generated from a platform that had no such
// fields) still hold byte-for-byte. Without omitempty each addition would break
// every one of them.
//
// It also matters on the wire in its own right: for type_ancestry, an ABSENT field
// and an EMPTY array would mean the same thing to a component but only one of them
// is what the platform sends, and a component asserting on `"type_ancestry": []`
// would be coding against a shape that never arrives.
func TestPlatformResolvedFields_AbsentDoNotAppearOnTheWire(t *testing.T) {
	raw, err := json.Marshal(Entity{ID: "e1", EntityType: "Equipment"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); got != `{"id":"e1","entity_type":"Equipment"}` {
		t.Errorf("entity marshalled as %s; an absent parent_refs and type_ancestry must both be omitted", got)
	}
	// An explicitly EMPTY chain is omitted too, which is what makes "absent" the one
	// representation of "no ancestry" rather than two a component has to handle.
	raw, err = json.Marshal(Entity{ID: "e1", EntityType: "Equipment", TypeAncestry: []string{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); got != `{"id":"e1","entity_type":"Equipment"}` {
		t.Errorf("entity marshalled as %s; an empty type_ancestry must be omitted too", got)
	}
}

func keysOf(m map[string]ParentRef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
