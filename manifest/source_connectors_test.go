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
		Name:  "fm-wo-connector",
		Image: "ghcr.io/ontogisai/oga-kit-built-environment/fm-wo-connector@sha256:abc",
		Bindings: []SourceBindingSpec{
			{ID: "wo-status", ExternalSystem: "contract_wo_mgmt", SourceType: "wo_status_feed", Modes: []string{"webhook"}},
			{ID: "wo-metrics", ExternalSystem: "contract_wo_mgmt", SourceType: "wo_metric_stream", Modes: []string{"poll"},
				TimeseriesMapping: &TimeseriesMappingSpec{EntityIDFrom: "source_id", MetricTag: "metric", UnitTag: "unit"}},
		},
	}})
	if err := Validate(m); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

// TestValidate_SourceConnectors_ContainerImage proves the canonical location
// for a connector image is container.image (OGA-637): a connector declaring
// the image ONLY under container.image validates, matching agents/MCP/loaders.
func TestValidate_SourceConnectors_ContainerImage(t *testing.T) {
	m := baseManifestWithConnectors([]SourceConnectorSpec{{
		Name:      "fm-wo-connector",
		Container: SidecarContainerSpec{Image: "ghcr.io/ontogisai/oga-kit-built-environment/fm-wo-connector@sha256:abc", Port: 8500},
		Bindings:  []SourceBindingSpec{{ID: "wo-status", ExternalSystem: "contract_wo_mgmt", SourceType: "wo_status_feed", Modes: []string{"webhook"}}},
	}})
	if err := Validate(m); err != nil {
		t.Fatalf("connector with container.image should be valid, got %v", err)
	}
}

// TestSourceConnectorSpec_EffectiveImage locks the precedence: container.image
// canonical, top-level image the back-compat fallback.
func TestSourceConnectorSpec_EffectiveImage(t *testing.T) {
	cases := []struct {
		name string
		spec SourceConnectorSpec
		want string
	}{
		{"container only", SourceConnectorSpec{Container: SidecarContainerSpec{Image: "c:2"}}, "c:2"},
		{"top-level only", SourceConnectorSpec{Image: "c:1"}, "c:1"},
		{"both → container wins", SourceConnectorSpec{Image: "c:1", Container: SidecarContainerSpec{Image: "c:2"}}, "c:2"},
		{"neither", SourceConnectorSpec{}, ""},
	}
	for _, tt := range cases {
		if got := tt.spec.EffectiveImage(); got != tt.want {
			t.Errorf("%s: EffectiveImage() = %q, want %q", tt.name, got, tt.want)
		}
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

// TestParse_RefusesCredentialRefs pins that the removed credential_refs field
// is refused on both roles that had it (OGA-1154): a kit declares a credential
// as a secret:// env value, and the strict parse names the field instead of
// dropping it, so a kit that still declares one finds out before upload.
func TestParse_RefusesCredentialRefs(t *testing.T) {
	head := `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
`
	cases := map[string]string{
		"source connector": `  source_connectors:
    - name: wo-connector
      container:
        image: ghcr.io/acme/wo-connector@sha256:abc
      bindings:
        - id: wo-status
          external_system: contract_wo_mgmt
          source_type: wo_status_feed
      credential_refs: [wo-mgmt-api-key]
`,
		"egress sync": `  egress_syncs:
    - name: example-egress
      external_system: ext-system
      entities_sync:
        - name: rec_Site
      credential_refs: [ext-system-api-key]
      container:
        image: ghcr.io/acme/example-egress@sha256:abc
`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(head + spec))
			if err == nil || !strings.Contains(err.Error(), "credential_refs") {
				t.Fatalf("Parse = %v, want an error naming credential_refs", err)
			}
		})
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

// TestValidate_SourceConnectors_OneSourceSystem: a connector's bindings name one
// external_system (record ownership): the platform owns what it writes as
// {kit}/{external_system}, so two would leave its records with no owner.
func TestValidate_SourceConnectors_OneSourceSystem(t *testing.T) {
	m := baseManifestWithConnectors([]SourceConnectorSpec{{
		Name:  "mixed",
		Image: "ghcr.io/ontogisai/kit/mixed@sha256:abc",
		Bindings: []SourceBindingSpec{
			{ID: "a", ExternalSystem: "system_a", SourceType: "feed_a"},
			{ID: "b", ExternalSystem: "system_b", SourceType: "feed_b"},
		},
	}})
	err := Validate(m)
	if err == nil || !strings.Contains(err.Error(), "one source system") {
		t.Fatalf("want a refusal naming one source system, got %v", err)
	}
	m.Spec.SourceConnectors[0].Bindings[1].ExternalSystem = " system_a "
	if err := Validate(m); err != nil {
		t.Fatalf("the same system, padded: want valid, got %v", err)
	}
}

func TestValidate_Loaders_RequireASourceSystem(t *testing.T) {
	m := baseManifestWithConnectors(nil)
	m.Spec.Loaders = []LoaderSpec{
		{Name: "types", Kind: LoaderKindOntology, ExternalSystem: "source_tool"},
		{Name: "records"},
	}
	err := Validate(m)
	if err == nil || !strings.Contains(err.Error(), "spec.loaders[1] (records): external_system is required") {
		t.Fatalf("want the loader without a source system refused, got %v", err)
	}
	m.Spec.Loaders[1].ExternalSystem = "source_tool"
	if err := Validate(m); err != nil {
		t.Fatalf("want valid, got %v", err)
	}
}

// TestParse_Loaders: spec.loaders decodes under the strict decoder, every field
// the platform reads included.
func TestParse_Loaders(t *testing.T) {
	m, err := Parse(strings.NewReader(`api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata: {name: kit, version: 1.0.0}
spec:
  platform_version: ">=1.0.0"
  loaders:
    - name: types
      kind: ontology
      description: {en-US: Types}
      container: {image: "ghcr.io/ontogisai/kit/types@sha256:abc", port: 8401}
      external_system: source_tool
      format: rdf
      formats: [rdf, ttl]
      data_path: ontology/types.ttl
      ontology_types_file: build/types.json
      governed_predicates_file: build/predicates.json
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if l := m.Spec.Loaders; len(l) != 1 || l[0].ExternalSystem != "source_tool" || l[0].Kind != LoaderKindOntology || l[0].Container.Port != 8401 {
		t.Fatalf("loaders = %+v", l)
	}
}
