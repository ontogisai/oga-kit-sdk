package transfer

import (
	"strings"
	"testing"
)

// TestValidateLocaleKeys_AcceptsFullBCP47 covers the happy path: full
// BCP-47 tags are accepted on EntityTypeDef-style locale maps.
func TestValidateLocaleKeys_AcceptsFullBCP47(t *testing.T) {
	cases := []map[string]string{
		nil,
		{},
		{"en-US": "x"},
		{"en-US": "x", "vi-VN": "y"},
		{"en-US": "x", "vi-VN": "y", "zh-CN": "z"},
		{"zh-Hant-TW": "x"}, // script + region
	}
	for i, m := range cases {
		if err := ValidateLocaleKeys("test.field", m); err != nil {
			t.Errorf("case %d: ValidateLocaleKeys returned error %v for valid map %v", i, err, m)
		}
	}
}

// TestValidateLocaleKeys_RejectsShortForms verifies the OGA-51 contract:
// short-form language-only tags MUST be rejected.
func TestValidateLocaleKeys_RejectsShortForms(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]string
	}{
		{"single en", map[string]string{"en": "x"}},
		{"single vi", map[string]string{"vi": "x"}},
		{"single zh", map[string]string{"zh": "x"}},
		{"mixed short and full", map[string]string{"en-US": "x", "vi": "y"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLocaleKeys("entity_type.display_name", tt.m)
			if err == nil {
				t.Fatalf("expected error for %v, got nil", tt.m)
			}
			if !strings.Contains(err.Error(), "entity_type.display_name") {
				t.Errorf("error %q should name the field", err)
			}
			if !strings.Contains(err.Error(), "short-form") {
				t.Errorf("error %q should mention short-form rejection", err)
			}
		})
	}
}

// TestValidateLocaleKeys_RejectsMalformed covers values that aren't
// valid BCP-47 at all.
func TestValidateLocaleKeys_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"underscore", "en_US"},
		{"trailing dash", "en-"},
		{"random", "!!!"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLocaleKeys("type_property.description", map[string]string{tt.key: "x"})
			if err == nil {
				t.Fatalf("expected error for malformed key %q", tt.key)
			}
			if !strings.Contains(err.Error(), "type_property.description") {
				t.Errorf("error %q should name the field", err)
			}
		})
	}
}

// TestValidateLocaleKeys_StableErrorOrdering verifies that error
// messages are deterministic across map iteration order — the helper
// sorts keys before walking them.
func TestValidateLocaleKeys_StableErrorOrdering(t *testing.T) {
	// Two short-form keys; whichever sorts first should be the one
	// the error names. "en" < "vi" lexically, so "en" wins.
	m := map[string]string{"vi": "x", "en": "y"}
	for range 5 {
		err := ValidateLocaleKeys("test", m)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), `"en"`) {
			t.Errorf("expected error to mention \"en\" (lexically first), got: %v", err)
		}
	}
}
