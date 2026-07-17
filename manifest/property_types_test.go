package manifest

import (
	"strings"
	"testing"
)

func TestSupportedPropertyType(t *testing.T) {
	supported := []string{
		"string", "text", "bool", "boolean",
		"int", "integer", "int64", "long", "int32",
		"float", "float64", "double", "number", "float32",
		"decimal", "datetime", "timestamp", "enum", "string_array",
		"geo_point", "geopoint", "point", "geo_polygon",
		"json", "object", "map", "ref", "reference",
		// case-insensitive + whitespace-trimmed:
		"FLOAT64", "  decimal ",
		// omitted type resolves to string on the platform → supported:
		"",
	}
	for _, tok := range supported {
		if !SupportedPropertyType(tok) {
			t.Errorf("SupportedPropertyType(%q) = false, want true", tok)
		}
	}

	unsupported := []string{"geo_multipolygon", "uuid", "vector", "bignum", "blob", "geo_line"}
	for _, tok := range unsupported {
		if SupportedPropertyType(tok) {
			t.Errorf("SupportedPropertyType(%q) = true, want false (fail-closed)", tok)
		}
	}
}

func TestValidatePropertyType(t *testing.T) {
	if err := ValidatePropertyType("Equipment", "actual_cost", "decimal"); err != nil {
		t.Errorf("representable type rejected: %v", err)
	}
	err := ValidatePropertyType("Parcel", "shape", "geo_multipolygon")
	if err == nil {
		t.Fatal("unrepresentable type must be rejected")
	}
	// The error names the token, owner, property, and the platform code.
	for _, want := range []string{"geo_multipolygon", "Parcel.shape", "OGA-DKIT-VAL-1032"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestSupportedPropertyTypeTokens_Sorted(t *testing.T) {
	toks := SupportedPropertyTypeTokens()
	if len(toks) == 0 {
		t.Fatal("expected a non-empty supported-token set")
	}
	for i := 1; i < len(toks); i++ {
		if toks[i-1] > toks[i] {
			t.Errorf("tokens not sorted at %d: %q > %q", i, toks[i-1], toks[i])
		}
	}
}
