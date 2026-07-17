package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// supportedPropertyTypes is the canonical set of ontology property-type tokens
// the platform can represent. It MIRRORS the platform's single canonical
// property-type table (oga-platform internal/domainkit/proptypes.go,
// propertyTypeTable, OGA-569) so a kit author gets the SAME fail-closed
// rejection locally that they would hit at install/upload time on a tenant's
// platform (the OGA-294 pattern for spec.policies, applied to ontology
// property types).
//
// Keys are the lowercased/trimmed token. The platform maps each of these to a
// concrete semantic ontology type AND an ArcadeDB DDL type; the SDK only needs
// to know which tokens are REPRESENTABLE, so it keeps the set (not the target
// types). When the platform table gains or drops a token, update this set in
// the same change — the two must not drift.
//
// A property that declares an unrepresentable type FAILS closed on the
// platform with OGA-DKIT-VAL-1032 (never a silent flatten to string); this set
// lets the kit's own CI catch that before publishing.
var supportedPropertyTypes = map[string]struct{}{
	// Text.
	"string": {}, "text": {},
	// Boolean.
	"bool": {}, "boolean": {},
	// Integers (platform stores all as LONG).
	"int": {}, "integer": {}, "int64": {}, "long": {},
	// Floats (float/float64/double/number → DOUBLE; float32 → FLOAT).
	"float": {}, "float64": {}, "double": {}, "number": {}, "float32": {},
	// Exact fixed-point (money) → DECIMAL.
	"decimal": {},
	// Temporal.
	"datetime": {}, "timestamp": {},
	// Constrained string (the manifest `values:` list is the constraint).
	"enum": {},
	// Ordered list of strings → ArcadeDB LIST.
	"string_array": {},
	// Geospatial (stored as GeoJSON/WKT text).
	"geo_point": {}, "geopoint": {}, "point": {}, "geo_polygon": {},
	// Structured JSON (stored as text).
	"json": {}, "object": {}, "map": {},
	// Reference to another entity (stored as the target id string).
	"ref": {}, "reference": {},
}

// SupportedPropertyTypeTokens returns the sorted set of representable property
// type tokens, for documentation and error messages.
func SupportedPropertyTypeTokens() []string {
	out := make([]string, 0, len(supportedPropertyTypes))
	for k := range supportedPropertyTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SupportedPropertyType reports whether the given kit property-type token is
// representable by the platform. Matching is case-insensitive and
// whitespace-trimmed. An empty/omitted type is reported as supported — an
// omission resolves to string on the platform and is not the "unrepresentable
// type" fail-closed case — mirroring resolvePropertyType.
func SupportedPropertyType(token string) bool {
	norm := strings.ToLower(strings.TrimSpace(token))
	if norm == "" {
		return true
	}
	_, ok := supportedPropertyTypes[norm]
	return ok
}

// ValidatePropertyType returns a descriptive error when token is a property
// type the platform cannot represent, naming the owning entity/relationship
// type and property so the kit author can find it. It returns nil for a
// representable (or empty) token.
//
// This is the kit-facing fail-closed check that mirrors the platform's
// OGA-DKIT-VAL-1032 rejection; the error message references that code so an
// author can cross-reference the platform behavior.
func ValidatePropertyType(ownerType, property, token string) error {
	if SupportedPropertyType(token) {
		return nil
	}
	return fmt.Errorf(
		"unsupported property type %q on %s.%s: the platform cannot represent it "+
			"and will not fall back to string (supported: %s) [OGA-DKIT-VAL-1032]",
		token, ownerType, property, strings.Join(SupportedPropertyTypeTokens(), ", "),
	)
}
