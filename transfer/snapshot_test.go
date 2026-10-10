package transfer_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// TestOntologySnapshot_JSONContract locks the snake_case wire keys of the
// snapshot envelope and its reused element types (OGA-636). The platform
// intake decodes this exact shape; a key rename here is a breaking wire change
// that this test surfaces.
func TestOntologySnapshot_JSONContract(t *testing.T) {
	snap := transfer.OntologySnapshot{
		EntityTypes: []transfer.EntityTypeDef{
			{
				Name:            "brick_ahu",
				DisplayName:     map[string]string{"en-US": "Air Handling Unit"},
				Description:     map[string]string{"en-US": "Brick AHU leaf class"},
				ParentType:      "Equipment",
				Category:        "equipment",
				Properties:      []transfer.TypeProperty{{Name: "capacity", Type: "float"}},
				Materialization: transfer.MaterializationLogical,
				PhysicalType:    "Equipment",
			},
		},
		RelationshipTypes: []transfer.RelationshipTypeDef{
			{
				Name:            "feeds",
				SourceType:      "*",
				TargetType:      "*",
				Cardinality:     "many_to_many",
				Materialization: transfer.MaterializationLogical,
				PhysicalType:    "RELATES",
			},
		},
		Hierarchy:        []transfer.HierarchyEntry{{TypeName: "brick_ahu", ParentType: "Equipment"}},
		GovernedPrefixes: []string{"brick_", "rec_"},
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	for _, want := range []string{
		`"entity_types"`,
		`"relationship_types"`,
		`"hierarchy"`,
		`"governed_prefixes"`,
		`"name":"brick_ahu"`,
		`"display_name":{"en-US":"Air Handling Unit"}`,
		`"parent_type":"Equipment"`,
		`"materialization":"logical"`,
		`"physical_type":"Equipment"`,
		`"type_name":"brick_ahu"`,
		`"source_type":"*"`,
		`"cardinality":"many_to_many"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot JSON missing %s\ngot: %s", want, got)
		}
	}

	// Round-trips back into the same struct.
	var back transfer.OntologySnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.EntityTypes) != 1 || back.EntityTypes[0].Name != "brick_ahu" ||
		back.EntityTypes[0].PhysicalType != "Equipment" {
		t.Errorf("entity round-trip lost fields: %+v", back.EntityTypes)
	}
	if len(back.Hierarchy) != 1 || back.Hierarchy[0].TypeName != "brick_ahu" {
		t.Errorf("hierarchy round-trip lost fields: %+v", back.Hierarchy)
	}
	if len(back.GovernedPrefixes) != 2 {
		t.Errorf("governed_prefixes round-trip lost entries: %+v", back.GovernedPrefixes)
	}
}

// TestOntologySnapshot_EmptyOmits verifies optional sections are omitted when
// empty so a minimal snapshot stays compact.
func TestOntologySnapshot_EmptyOmits(t *testing.T) {
	raw, err := json.Marshal(transfer.OntologySnapshot{
		EntityTypes: []transfer.EntityTypeDef{{Name: "brick_ahu"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	for _, absent := range []string{"relationship_types", "governed_prefixes", "hierarchy"} {
		if strings.Contains(got, absent) {
			t.Errorf("expected %q omitted for a minimal snapshot\ngot: %s", absent, got)
		}
	}
}
