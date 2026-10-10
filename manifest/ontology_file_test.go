package manifest

import (
	"strings"
	"testing"
)

func TestValidateOntologyFile_Representable(t *testing.T) {
	data := []byte(`
spec:
  entity_types:
    - name: Equipment
      display_name: { en-US: "Equipment" }
      h3_config: { indexed: true }
      properties:
        - {name: latitude, type: float64}
        - {name: longitude, type: float64}
        - {name: actual_cost, type: decimal}
        - {name: tags, type: string_array}
        - {name: status, type: enum, values: [ok, fault]}
        - {name: footprint, type: geo_polygon}
        - {name: name, type: string}
  relationship_types:
    - name: SERVES
      source_type: Equipment
      target_type: Equipment
      properties:
        - {name: since, type: datetime}
`)
	if err := ValidateOntologyFile(data); err != nil {
		t.Fatalf("fully-representable ontology file rejected: %v", err)
	}
}

func TestValidateOntologyFile_UnsupportedEntityProp(t *testing.T) {
	data := []byte(`
spec:
  entity_types:
    - name: Parcel
      properties:
        - {name: shape, type: geo_multipolygon}
`)
	err := ValidateOntologyFile(data)
	if err == nil {
		t.Fatal("unrepresentable entity property type must be rejected")
	}
	if !strings.Contains(err.Error(), "Parcel.shape") || !strings.Contains(err.Error(), "geo_multipolygon") {
		t.Errorf("error should name the offending property/type: %v", err)
	}
}

func TestValidateOntologyFile_UnsupportedRelationshipProp(t *testing.T) {
	// Covers the relationship-property path the platform's DDL check (entities
	// only) does not — mirrors buildOntologyFromKitFiles.
	data := []byte(`
spec:
  entity_types:
    - {name: A}
    - {name: B}
  relationship_types:
    - name: LINKS
      source_type: A
      target_type: B
      properties:
        - {name: weightish, type: bignum}
`)
	err := ValidateOntologyFile(data)
	if err == nil {
		t.Fatal("unrepresentable relationship property type must be rejected")
	}
	if !strings.Contains(err.Error(), "LINKS.weightish") {
		t.Errorf("error should name the relationship property: %v", err)
	}
}

func TestValidateOntologyFile_EmptyAndUntyped(t *testing.T) {
	// No spec / no properties / omitted type are all tolerated.
	for _, data := range [][]byte{
		[]byte(``),
		[]byte("spec: {}\n"),
		[]byte("spec:\n  entity_types:\n    - name: X\n      properties:\n        - {name: note}\n"),
	} {
		if err := ValidateOntologyFile(data); err != nil {
			t.Errorf("ValidateOntologyFile(%q) = %v, want nil", string(data), err)
		}
	}
}
