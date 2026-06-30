package manifest

import (
	"strings"
	"testing"
)

func baseManifestWithConnectors(conns []SourceConnectorSpec) *KitManifest {
	return &KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata:   KitMetadata{Name: "kit", Version: "1.0.0"},
		Spec:       KitSpec{PlatformVersion: ">=1.0.0", SourceConnectors: conns},
	}
}

func TestValidate_SourceConnectors_Valid(t *testing.T) {
	m := baseManifestWithConnectors([]SourceConnectorSpec{{
		Name:           "fm-wo-connector",
		Image:          "ghcr.io/ontogisai/oga-kit-built-environment/fm-wo-connector@sha256:abc",
		CredentialRefs: []string{"connector-wo-api-key"},
		Bindings: []SourceBindingSpec{
			{ID: "wo-status", ExternalSystem: "contract_wo_mgmt", SourceType: "wo_status_feed", Modes: []string{"webhook"}},
			{ID: "bms", ExternalSystem: "acme_bms", SourceType: "bms_point_stream", Modes: []string{"poll"},
				TimeseriesMapping: &TimeseriesMappingSpec{EntityIDFrom: "source_id", MetricTag: "metric", UnitTag: "unit"}},
		},
	}})
	if err := Validate(m); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

// TestParse_SourceConnectorContainerEnv proves the strict decoder (Parse uses
// KnownFields(true)) accepts a source connector's container.env block and
// decodes it — so a kit can deliver per-tenant connector config such as
// WO_MGMT_URL via a secret:// reference (continuous-ingress-connectors,
// OGA-437) without the strict parse rejecting the unknown field.
func TestParse_SourceConnectorContainerEnv(t *testing.T) {
	y := `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  source_connectors:
    - name: wo-connector
      image: ghcr.io/ontogisai/oga-kit-built-environment/wo-connector:1.0.0
      container:
        env:
          WO_MGMT_URL: "secret://wo-mgmt-url"
          WO_POLL_INTERVAL: "30s"
      bindings:
        - id: wo-status
          external_system: contract_wo_mgmt
          source_type: wo_status_feed
          modes: [poll, webhook]
      credential_refs:
        - wo-mgmt-api-key
`
	m, err := Parse(strings.NewReader(y))
	if err != nil {
		t.Fatalf("strict Parse rejected container.env on a source connector: %v", err)
	}
	if len(m.Spec.SourceConnectors) != 1 {
		t.Fatalf("source_connectors = %d, want 1", len(m.Spec.SourceConnectors))
	}
	c := m.Spec.SourceConnectors[0]
	if got := c.Container.Env["WO_MGMT_URL"]; got != "secret://wo-mgmt-url" {
		t.Errorf("container.env WO_MGMT_URL = %q, want secret://wo-mgmt-url", got)
	}
	if got := c.Container.Env["WO_POLL_INTERVAL"]; got != "30s" {
		t.Errorf("container.env WO_POLL_INTERVAL = %q, want 30s", got)
	}
}

func TestValidate_SourceConnectors_Errors(t *testing.T) {
	cases := []struct {
		name  string
		conns []SourceConnectorSpec
		want  string
	}{
		{"missing name", []SourceConnectorSpec{{Image: "img", Bindings: []SourceBindingSpec{{ID: "a", ExternalSystem: "s", SourceType: "t"}}}}, "name is required"},
		{"missing image", []SourceConnectorSpec{{Name: "c", Bindings: []SourceBindingSpec{{ID: "a", ExternalSystem: "s", SourceType: "t"}}}}, "image is required"},
		{"no bindings", []SourceConnectorSpec{{Name: "c", Image: "img"}}, "at least one binding"},
		{"binding missing id", []SourceConnectorSpec{{Name: "c", Image: "img", Bindings: []SourceBindingSpec{{ExternalSystem: "s", SourceType: "t"}}}}, "id is required"},
		{"binding missing external_system", []SourceConnectorSpec{{Name: "c", Image: "img", Bindings: []SourceBindingSpec{{ID: "a", SourceType: "t"}}}}, "external_system is required"},
		{"binding missing source_type", []SourceConnectorSpec{{Name: "c", Image: "img", Bindings: []SourceBindingSpec{{ID: "a", ExternalSystem: "s"}}}}, "source_type is required"},
		{"invalid mode", []SourceConnectorSpec{{Name: "c", Image: "img", Bindings: []SourceBindingSpec{{ID: "a", ExternalSystem: "s", SourceType: "t", Modes: []string{"stream"}}}}}, "mode = \"stream\" is invalid"},
		{"duplicate binding id", []SourceConnectorSpec{{Name: "c", Image: "img", Bindings: []SourceBindingSpec{
			{ID: "a", ExternalSystem: "s", SourceType: "t"},
			{ID: "a", ExternalSystem: "s2", SourceType: "t2"},
		}}}, "duplicates bindings"},
		{"duplicate connector name", []SourceConnectorSpec{
			{Name: "c", Image: "img", Bindings: []SourceBindingSpec{{ID: "a", ExternalSystem: "s", SourceType: "t"}}},
			{Name: "c", Image: "img2", Bindings: []SourceBindingSpec{{ID: "b", ExternalSystem: "s", SourceType: "t"}}},
		}, "duplicates spec.source_connectors"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(baseManifestWithConnectors(tt.conns))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should contain %q", err.Error(), tt.want)
			}
		})
	}
}

// TestParse_RejectsIngestionTemplates verifies the clean-cut removal: a
// manifest still declaring the old ingestion_templates field is rejected by
// the strict parser (KnownFields), forcing migration to source_connectors.
func TestParse_RejectsIngestionTemplates(t *testing.T) {
	const y = `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  ingestion_templates:
    - ingestion-templates/ifc-import.yaml
`
	if _, err := Parse(strings.NewReader(y)); err == nil {
		t.Fatal("expected strict parse to reject the removed ingestion_templates field")
	}
}
