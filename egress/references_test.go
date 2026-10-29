package egress

import (
	"encoding/json"
	"reflect"
	"testing"
)

// platformReferencesRequestJSON is an entity-lane request carrying references
// (OGA-1190), marshalled from the platform's internal/egress.SyncRequest.
//
// RECONCILED like platformResolvedRefsRequestJSON: the platform holds the same
// bytes in internal/egress/references_wire_test.go and asserts that marshalling
// produces them. Keep the two copies identical. It pins the field ORDER too:
// references sits between parent_refs and type_ancestry.
//
// The entities cover the shapes a component handles: two references under one
// name (ordered by entity_id), a reference with an empty label, and an entity
// with none, where the field is OMITTED rather than sent as {}.
const platformReferencesRequestJSON = `{
  "tenant_id": "tnt2",
  "external_system": "ext-system",
  "entity_type": "Equipment",
  "mode": "bulk",
  "batch_id": "tnt2:example-egress-sync:Equipment:bulk:1:abc",
  "entities": [
    {
      "id": "019e38e3-camera",
      "entity_type": "brick:Surveillance_Camera",
      "parent_refs": {
        "hasLocation": {
          "entity_id": "019e38e3-room",
          "external_record_id": "8f2c1d04-ext-uuid"
        }
      },
      "references": {
        "isPartOf": [
          {
            "entity_id": "019e38e3-sys-a",
            "entity_type": "brick:System",
            "label": "CCTV System"
          },
          {
            "entity_id": "019e38e3-sys-b",
            "entity_type": "brick:Collection",
            "label": ""
          }
        ]
      },
      "type_ancestry": [
        "brick:Surveillance_Camera",
        "brick:Equipment"
      ]
    },
    {
      "id": "019e38e3-pump",
      "entity_type": "brick:Pump"
    }
  ]
}`

func TestSyncRequest_DecodesPlatformReferences(t *testing.T) {
	var req SyncRequest
	if err := json.Unmarshal([]byte(platformReferencesRequestJSON), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(req.Entities))
	}
	want := []Reference{
		{EntityID: "019e38e3-sys-a", EntityType: "brick:System", Label: "CCTV System"},
		{EntityID: "019e38e3-sys-b", EntityType: "brick:Collection", Label: ""},
	}
	if got := req.Entities[0].References["isPartOf"]; !reflect.DeepEqual(got, want) {
		t.Errorf("references[isPartOf] = %+v, want %+v", got, want)
	}
	if got := req.Entities[1].References; got != nil {
		t.Errorf("an entity with no references decoded %+v, want nil", got)
	}
}

// Re-marshalling pins the field order against the platform's copy.
func TestSyncRequest_RoundTripsPlatformReferences(t *testing.T) {
	var req SyncRequest
	if err := json.Unmarshal([]byte(platformReferencesRequestJSON), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != platformReferencesRequestJSON {
		t.Errorf("round-tripped request differs from the platform's wire:\n got: %s\nwant: %s",
			got, platformReferencesRequestJSON)
	}
}

func TestReferenceTags_AreWireLiterals(t *testing.T) {
	f, ok := reflect.TypeOf(Entity{}).FieldByName("References")
	if !ok {
		t.Fatal("Entity has no field References")
	}
	if got := f.Tag.Get("json"); got != "references,omitempty" {
		t.Errorf("Entity.References json tag = %q, want %q", got, "references,omitempty")
	}
	for field, want := range map[string]string{
		"EntityID":   "entity_id",
		"EntityType": "entity_type",
		"Label":      "label",
	} {
		f, ok := reflect.TypeOf(Reference{}).FieldByName(field)
		if !ok {
			t.Errorf("Reference has no field %s", field)
			continue
		}
		if got := f.Tag.Get("json"); got != want {
			t.Errorf("Reference.%s json tag = %q, want %q", field, got, want)
		}
	}
}
