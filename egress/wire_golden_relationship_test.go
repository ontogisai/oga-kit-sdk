package egress

import (
	"encoding/json"
	"slices"
	"testing"
)

// Cross-repo wire drift guard for the RELATIONSHIPS lane, SDK side (OGA-875).
//
// This literal is BYTE-IDENTICAL to the platform's
// internal/egress/wire_golden_relationship_test.go
// (platformRelationshipSyncRequestJSON). The two repos declare
// RelationshipSyncRequest / Relationship / RelationshipEndpoint independently,
// and a component decodes exactly these bytes — so a renamed JSON tag or a
// reordered field on either side must fail on that side, naming the bytes it
// expected. Keep the two copies in lockstep.
//
// ⚠️ The batch_id's `\u003e` is Go encoding/json's HTML escaping of `>` (the
// platform's relationships-lane batch label carries the scope tuple
// predicate>source_type>target_type). It decodes back to `>`, so the dedup key
// round-trips exactly.
const sharedRelationshipSyncRequestJSON = `{
  "tenant_id": "tnt2",
  "external_system": "ext-system",
  "predicate": "feeds",
  "mode": "bulk",
  "batch_id": "tnt2:example-egress-sync:feeds\u003eEquipment\u003eLocation:bulk:relationships:#0",
  "relationships": [
    {
      "id": "edge-1",
      "predicate": "feeds",
      "source": {
        "entity_id": "eq-1",
        "entity_type": "brick:AHU",
        "type_ancestry": [
          "brick:AHU",
          "brick:HVAC_Equipment",
          "Equipment"
        ],
        "correlation": {
          "external_system": "ext-system",
          "external_record_id": "EXT-EQ-1"
        }
      },
      "target": {
        "entity_id": "loc-1",
        "entity_type": "rec:HVACZone",
        "type_ancestry": [
          "rec:HVACZone",
          "rec:Zone",
          "Location"
        ],
        "correlation": {
          "external_system": "ext-system",
          "external_record_id": "EXT-LOC-1"
        }
      },
      "correlation": {
        "external_system": "ext-system",
        "external_record_id": "EXT-REL-1"
      }
    },
    {
      "id": "edge-2",
      "predicate": "feeds",
      "properties": {
        "flow_rate": "high"
      },
      "source": {
        "entity_id": "eq-2",
        "entity_type": "brick:Fan",
        "correlation": {
          "external_system": "ext-system",
          "external_record_id": "EXT-EQ-2"
        }
      },
      "target": {
        "entity_id": "loc-1",
        "entity_type": "rec:HVACZone",
        "type_ancestry": [
          "rec:HVACZone",
          "rec:Zone",
          "Location"
        ],
        "correlation": {
          "external_system": "ext-system",
          "external_record_id": "EXT-LOC-1"
        }
      }
    }
  ]
}`

func sharedRelationshipSyncRequestFixture() *RelationshipSyncRequest {
	return &RelationshipSyncRequest{
		TenantID:       "tnt2",
		ExternalSystem: "ext-system",
		Predicate:      "feeds",
		Mode:           ModeBulk,
		BatchID:        "tnt2:example-egress-sync:feeds>Equipment>Location:bulk:relationships:#0",
		Relationships: []Relationship{
			{
				ID:        "edge-1",
				Predicate: "feeds",
				Source: RelationshipEndpoint{
					EntityID: "eq-1", EntityType: "brick:AHU",
					TypeAncestry: []string{"brick:AHU", "brick:HVAC_Equipment", "Equipment"},
					Correlation:  &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-EQ-1"},
				},
				Target: RelationshipEndpoint{
					EntityID: "loc-1", EntityType: "rec:HVACZone",
					TypeAncestry: []string{"rec:HVACZone", "rec:Zone", "Location"},
					Correlation:  &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-LOC-1"},
				},
				Correlation: &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-REL-1"},
			},
			{
				ID:         "edge-2",
				Predicate:  "feeds",
				Properties: map[string]any{"flow_rate": "high"},
				Source: RelationshipEndpoint{
					EntityID: "eq-2", EntityType: "brick:Fan",
					Correlation: &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-EQ-2"},
				},
				Target: RelationshipEndpoint{
					EntityID: "loc-1", EntityType: "rec:HVACZone",
					TypeAncestry: []string{"rec:HVACZone", "rec:Zone", "Location"},
					Correlation:  &Correlation{ExternalSystem: "ext-system", ExternalRecordID: "EXT-LOC-1"},
				},
			},
		},
	}
}

// The SDK must SERIALIZE exactly the bytes the platform's fixture expects.
func TestRelationshipSyncRequest_MarshalsToTheSharedWire(t *testing.T) {
	got, err := json.MarshalIndent(sharedRelationshipSyncRequestFixture(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != sharedRelationshipSyncRequestJSON {
		t.Errorf("SDK relationship wire differs from the shared fixture (and therefore from the platform's copy):\n got: %s\nwant: %s",
			got, sharedRelationshipSyncRequestJSON)
	}
}

// And the reverse: the platform's bytes decode into the SDK structs unchanged
// — the direction that catches a tag rename (a mismatched tag decodes to a
// zero value with no error, so an endpoint's foreign key would silently vanish).
func TestRelationshipSyncRequest_DecodesTheSharedWire(t *testing.T) {
	var req RelationshipSyncRequest
	if err := json.Unmarshal([]byte(sharedRelationshipSyncRequestJSON), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Relationships) != 2 {
		t.Fatalf("relationships = %d, want 2", len(req.Relationships))
	}
	edge1 := req.Relationships[0]
	if edge1.Source.Correlation == nil || edge1.Source.Correlation.ExternalRecordID != "EXT-EQ-1" {
		t.Errorf("edge-1 source correlation = %+v, want EXT-EQ-1", edge1.Source.Correlation)
	}
	if edge1.Target.Correlation == nil || edge1.Target.Correlation.ExternalRecordID != "EXT-LOC-1" {
		t.Errorf("edge-1 target correlation = %+v, want EXT-LOC-1", edge1.Target.Correlation)
	}
	if edge1.Correlation == nil || edge1.Correlation.ExternalRecordID != "EXT-REL-1" {
		t.Errorf("edge-1 own correlation = %+v, want EXT-REL-1", edge1.Correlation)
	}
	if req.BatchID != "tnt2:example-egress-sync:feeds>Equipment>Location:bulk:relationships:#0" {
		t.Errorf("batch_id = %q; the \\u003e must decode back to > for the dedup key to round-trip", req.BatchID)
	}
	if edge2 := req.Relationships[1]; edge2.Correlation != nil {
		t.Errorf("edge-2 own correlation = %+v, want nil (the create case)", edge2.Correlation)
	}
	// type_ancestry: present and leaf first where the ontology defines the class,
	// absent (nil, never a one-element guess) where it does not (OGA-1252).
	want := []string{"brick:AHU", "brick:HVAC_Equipment", "Equipment"}
	if got := edge1.Source.TypeAncestry; !slices.Equal(got, want) {
		t.Errorf("edge-1 source type_ancestry = %v, want %v", got, want)
	}
	if got := req.Relationships[1].Source.TypeAncestry; got != nil {
		t.Errorf("edge-2 source type_ancestry = %v, want nil (the class the ontology does not define)", got)
	}
}

// An endpoint whose class the ontology does not define must marshal WITHOUT the
// type_ancestry key: absence is the one wire representation of "no ancestry",
// so neither a nil nor an empty chain may serialize as [] or null (OGA-1252).
func TestRelationshipEndpoint_AbsentTypeAncestryIsOmitted(t *testing.T) {
	for _, ancestry := range [][]string{nil, {}} {
		raw, err := json.Marshal(RelationshipEndpoint{EntityID: "a", EntityType: "X", TypeAncestry: ancestry})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, present := m["type_ancestry"]; present {
			t.Errorf("an endpoint with type_ancestry %#v serialized the key: %s", ancestry, raw)
		}
	}
}
