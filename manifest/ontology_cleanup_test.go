package manifest

import (
	"strings"
	"testing"
)

const ontologyCleanupManifest = `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  source_connectors:
    - name: register-pull
      container:
        image: ghcr.io/ontogisai/example/register-pull@sha256:abc
      bindings:
        - id: register
          external_system: asset-register
          source_type: register_records
          modes: [poll]
%s
`

// TestParse_OntologyCleanup pins the opt-in's decoding (connector class
// lifecycle): the strict parse accepts ontology_cleanup on a source connector,
// and a connector that leaves it out decodes with the cleanup off.
func TestParse_OntologyCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{name: "declared", line: "      ontology_cleanup: remove_empty_omitted", want: OntologyCleanupRemoveEmptyOmitted},
		{name: "absent", line: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(strings.NewReader(strings.Replace(ontologyCleanupManifest, "%s", tc.line, 1)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := m.Spec.SourceConnectors[0].OntologyCleanup; got != tc.want {
				t.Errorf("OntologyCleanup = %q, want %q", got, tc.want)
			}
			if err := Validate(m); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

// TestValidate_OntologyCleanupRefusesAnUnknownValue: only the one mode is
// accepted, so a misspelt value is an error rather than a silent "off" that
// leaves the kit author believing the cleanup is on.
func TestValidate_OntologyCleanupRefusesAnUnknownValue(t *testing.T) {
	for _, value := range []string{"remove", "Remove_Empty_Omitted", "true", "off"} {
		m := baseManifestWithConnectors([]SourceConnectorSpec{{
			Name:            "register-pull",
			Container:       SidecarContainerSpec{Image: "ghcr.io/ontogisai/example/register-pull@sha256:abc"},
			Bindings:        []SourceBindingSpec{{ID: "register", ExternalSystem: "asset-register", SourceType: "register_records", Modes: []string{"poll"}}},
			OntologyCleanup: value,
		}})
		err := Validate(m)
		if err == nil {
			t.Errorf("ontology_cleanup = %q validated; want it refused", value)
			continue
		}
		if !strings.Contains(err.Error(), "ontology_cleanup") || !strings.Contains(err.Error(), OntologyCleanupRemoveEmptyOmitted) {
			t.Errorf("ontology_cleanup = %q: error %q should name the field and the accepted value", value, err)
		}
	}
}

// TestParse_OntologyCleanupStaysStrict: adding the field leaves the strict
// decoder strict, so a misspelt key is still refused rather than dropped.
func TestParse_OntologyCleanupStaysStrict(t *testing.T) {
	y := strings.Replace(ontologyCleanupManifest, "%s", "      ontology_clean_up: remove_empty_omitted", 1)
	if _, err := Parse(strings.NewReader(y)); err == nil {
		t.Error("Parse accepted the unknown key ontology_clean_up")
	}
}
