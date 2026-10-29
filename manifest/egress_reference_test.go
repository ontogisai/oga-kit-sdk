package manifest

import (
	"strings"
	"testing"
)

// referenceManifest wraps an Equipment entry carrying refs in an otherwise valid
// manifest. delivery is the component-level change_delivery line ("" for none).
func referenceManifest(refs, delivery string) string {
	return strings.Replace(egressManifestWithEntitiesSync(`        - name: Equipment
          include_descendants: true
          reference_edges:
`+refs), "      container:\n", delivery+"      container:\n", 1)
}

const offDelivery = "      change_delivery: \"off\"\n"

func TestParse_ReferenceEdges(t *testing.T) {
	m, err := Parse(strings.NewReader(referenceManifest(`            - edge: hasPart
              direction: in
              target_type: System
              name: isPartOf
            - edge: hasPart
              direction: out
              target_type: Equipment
              name: hasParts
`, offDelivery)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	refs := m.Spec.EgressSyncs[0].EntitiesSync[0].ReferenceEdges
	want := []ReferenceEdgeSpec{
		{Edge: "hasPart", Direction: ParentEdgeIn, TargetType: "System", Name: "isPartOf"},
		{Edge: "hasPart", Direction: ParentEdgeOut, TargetType: "Equipment", Name: "hasParts"},
	}
	if len(refs) != len(want) {
		t.Fatalf("reference_edges = %+v, want %+v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("reference_edges[%d] = %+v, want %+v", i, refs[i], want[i])
		}
	}
}

// The decoder refuses what it cannot represent faithfully: a mistyped key would
// otherwise be dropped (Node.Decode ignores KnownFields), and a scalar has no
// direction to state.
func TestParse_ReferenceEdgesRefusesUnknownKeyAndScalar(t *testing.T) {
	cases := []struct {
		name, refs, want string
	}{
		{"mistyped key", `            - edge: hasPart
              directon: in
              target_type: System
              name: isPartOf
`, "directon"},
		{"scalar entry", `            - hasPart
`, "must be a mapping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(referenceManifest(tc.refs, offDelivery)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateEgressSyncs_ReferenceEdges(t *testing.T) {
	valid := ReferenceEdgeSpec{Edge: "hasPart", Direction: ParentEdgeIn, TargetType: "System", Name: "isPartOf"}
	cases := []struct {
		name     string
		refs     []ReferenceEdgeSpec
		delivery ChangeDelivery
		want     string // "" means accepted
	}{
		{"valid", []ReferenceEdgeSpec{valid}, ChangeDeliveryOff, ""},
		{"same edge under two names", []ReferenceEdgeSpec{valid,
			{Edge: "hasPart", Direction: ParentEdgeOut, TargetType: "Equipment", Name: "hasParts"}},
			ChangeDeliveryOff, ""},
		{"missing direction", []ReferenceEdgeSpec{{Edge: "hasPart", TargetType: "System", Name: "isPartOf"}},
			ChangeDeliveryOff, "all required"},
		{"missing target_type", []ReferenceEdgeSpec{{Edge: "hasPart", Direction: ParentEdgeIn, Name: "isPartOf"}},
			ChangeDeliveryOff, "all required"},
		{"missing name", []ReferenceEdgeSpec{{Edge: "hasPart", Direction: ParentEdgeIn, TargetType: "System"}},
			ChangeDeliveryOff, "all required"},
		{"unknown direction", []ReferenceEdgeSpec{{Edge: "hasPart", Direction: "up", TargetType: "System", Name: "isPartOf"}},
			ChangeDeliveryOff, "direction"},
		{"namespaced edge", []ReferenceEdgeSpec{{Edge: "rec:hasPart", Direction: ParentEdgeIn, TargetType: "System", Name: "isPartOf"}},
			ChangeDeliveryOff, `declare "rec_hasPart"`},
		{"illegal name", []ReferenceEdgeSpec{{Edge: "hasPart", Direction: ParentEdgeIn, TargetType: "System", Name: "is-part-of"}},
			ChangeDeliveryOff, "name"},
		{"duplicate name", []ReferenceEdgeSpec{valid, valid}, ChangeDeliveryOff, "declared twice"},
		{"event-fed component", []ReferenceEdgeSpec{valid}, ChangeDeliveryEvents, "change_delivery"},
		{"default delivery is events", []ReferenceEdgeSpec{valid}, "", "change_delivery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			syncs := []EgressSyncSpec{{
				Name:           "example-egress-sync",
				ExternalSystem: "ext-system",
				EntitiesSync:   []EgressEntityTypeSpec{{Name: "Equipment", ReferenceEdges: tc.refs}},
				Container:      SidecarContainerSpec{Image: "ghcr.io/ontogisai/x@sha256:abc"},
				ChangeDelivery: tc.delivery,
			}}
			err := validateEgressSyncs(syncs)
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
