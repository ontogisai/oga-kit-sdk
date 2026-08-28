package egress

import (
	"encoding/json"
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
  "tenant_id": "sjcs1",
  "external_system": "24k-core",
  "predicate": "feeds",
  "mode": "bulk",
  "batch_id": "sjcs1:core-egress-sync:feeds\u003eEquipment\u003eLocation:bulk:relationships:#0",
  "relationships": [
    {
      "id": "edge-1",
      "predicate": "feeds",
      "source": {
        "entity_id": "eq-1",
        "entity_type": "brick:AHU",
        "correlation": {
          "external_system": "24k-core",
          "external_record_id": "CORE-EQ-1"
        }
      },
      "target": {
        "entity_id": "loc-1",
        "entity_type": "rec:HVACZone",
        "correlation": {
          "external_system": "24k-core",
          "external_record_id": "CORE-LOC-1"
        }
      },
      "correlation": {
        "external_system": "24k-core",
        "external_record_id": "CORE-REL-1"
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
          "external_system": "24k-core",
          "external_record_id": "CORE-EQ-2"
        }
      },
      "target": {
        "entity_id": "loc-1",
        "entity_type": "rec:HVACZone",
        "correlation": {
          "external_system": "24k-core",
          "external_record_id": "CORE-LOC-1"
        }
      }
    }
  ]
}`

func sharedRelationshipSyncRequestFixture() *RelationshipSyncRequest {
	return &RelationshipSyncRequest{
		TenantID:       "sjcs1",
		ExternalSystem: "24k-core",
		Predicate:      "feeds",
		Mode:           ModeBulk,
		BatchID:        "sjcs1:core-egress-sync:feeds>Equipment>Location:bulk:relationships:#0",
		Relationships: []Relationship{
			{
				ID:        "edge-1",
				Predicate: "feeds",
				Source: RelationshipEndpoint{
					EntityID: "eq-1", EntityType: "brick:AHU",
					Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-EQ-1"},
				},
				Target: RelationshipEndpoint{
					EntityID: "loc-1", EntityType: "rec:HVACZone",
					Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-LOC-1"},
				},
				Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-REL-1"},
			},
			{
				ID:         "edge-2",
				Predicate:  "feeds",
				Properties: map[string]any{"flow_rate": "high"},
				Source: RelationshipEndpoint{
					EntityID: "eq-2", EntityType: "brick:Fan",
					Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-EQ-2"},
				},
				Target: RelationshipEndpoint{
					EntityID: "loc-1", EntityType: "rec:HVACZone",
					Correlation: &Correlation{ExternalSystem: "24k-core", ExternalRecordID: "CORE-LOC-1"},
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
	if edge1.Source.Correlation == nil || edge1.Source.Correlation.ExternalRecordID != "CORE-EQ-1" {
		t.Errorf("edge-1 source correlation = %+v, want CORE-EQ-1", edge1.Source.Correlation)
	}
	if edge1.Target.Correlation == nil || edge1.Target.Correlation.ExternalRecordID != "CORE-LOC-1" {
		t.Errorf("edge-1 target correlation = %+v, want CORE-LOC-1", edge1.Target.Correlation)
	}
	if edge1.Correlation == nil || edge1.Correlation.ExternalRecordID != "CORE-REL-1" {
		t.Errorf("edge-1 own correlation = %+v, want CORE-REL-1", edge1.Correlation)
	}
	if req.BatchID != "sjcs1:core-egress-sync:feeds>Equipment>Location:bulk:relationships:#0" {
		t.Errorf("batch_id = %q; the \\u003e must decode back to > for the dedup key to round-trip", req.BatchID)
	}
	if edge2 := req.Relationships[1]; edge2.Correlation != nil {
		t.Errorf("edge-2 own correlation = %+v, want nil (the create case)", edge2.Correlation)
	}
}
