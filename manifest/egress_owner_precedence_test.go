package manifest

import (
	"slices"
	"strings"
	"testing"
)

// owner_precedence parses from the mapping form, keeps its order, and validates
// when every entry is one of the component's entities_sync names.
func TestParse_OwnerPrecedence(t *testing.T) {
	y := egressManifestWithEntitiesSync(`        - name: Location
          parent_edges: [hasLocation]
          hierarchical: true
        - name: Equipment
          parent_edges: [hasLocation]
        - name: Point
          parent_edges:
            - edge: hasPoint
              direction: in
              owner_precedence: [Equipment, Location]
`)
	m, err := Parse(strings.NewReader(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	pe := m.Spec.EgressSyncs[0].EntitiesSync[2].ParentEdges[0]
	if want := []string{"Equipment", "Location"}; !slices.Equal(pe.OwnerPrecedence, want) {
		t.Errorf("owner_precedence = %v, want %v", pe.OwnerPrecedence, want)
	}
	if pe.EffectiveDirection() != ParentEdgeIn {
		t.Errorf("direction = %q, want %q", pe.EffectiveDirection(), ParentEdgeIn)
	}
}

// A scalar is not a list: the decoder refuses it rather than reading one entry.
func TestParse_OwnerPrecedenceRejectsScalar(t *testing.T) {
	y := egressManifestWithEntitiesSync(`        - name: Equipment
        - name: Point
          parent_edges:
            - edge: hasPoint
              direction: in
              owner_precedence: Equipment
`)
	if _, err := Parse(strings.NewReader(y)); err == nil || !strings.Contains(err.Error(), "owner_precedence") {
		t.Fatalf("Parse error = %v, want one naming owner_precedence", err)
	}
}

func TestValidateEgressSyncs_OwnerPrecedence(t *testing.T) {
	point := func(precedence ...string) EgressEntityTypeSpec {
		return EgressEntityTypeSpec{Name: "Point", ParentEdges: []ParentEdgeSpec{
			{Edge: "hasPoint", Direction: ParentEdgeIn, OwnerPrecedence: precedence},
		}}
	}
	cases := []struct {
		name  string
		types []EgressEntityTypeSpec
		want  string // "" ⇒ valid
	}{
		{
			name:  "two declared types",
			types: []EgressEntityTypeSpec{{Name: "Equipment"}, {Name: "Location"}, point("Equipment", "Location")},
		},
		{
			name:  "one entry is a filter",
			types: []EgressEntityTypeSpec{{Name: "Equipment"}, point("Equipment")},
		},
		{
			name:  "entry not declared",
			types: []EgressEntityTypeSpec{{Name: "Equipment"}, point("Equipment", "Location")},
			want:  `"Location" is not one of this component's entities_sync names`,
		},
		{
			name:  "empty entry",
			types: []EgressEntityTypeSpec{{Name: "Equipment"}, point("Equipment", "")},
			want:  `"" is not one of this component's entities_sync names`,
		},
		{
			name:  "entry listed twice",
			types: []EgressEntityTypeSpec{{Name: "Equipment"}, point("Equipment", "Equipment")},
			want:  `lists "Equipment" twice`,
		},
		{
			name: "hierarchical type",
			types: []EgressEntityTypeSpec{{
				Name: "Location", Hierarchical: true,
				ParentEdges: []ParentEdgeSpec{{Edge: "hasLocation", OwnerPrecedence: []string{"Location"}}},
			}},
			want: "not allowed on a hierarchical type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := EgressSyncSpec{
				Name: "example-sync", ExternalSystem: "ext-system",
				EntitiesSync: tc.types,
				Container:    SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
			}
			err := validateEgressSyncs([]EgressSyncSpec{e})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
