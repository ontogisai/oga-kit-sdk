package manifest

import (
	"strings"
	"testing"
)

func TestParse_ValidManifest(t *testing.T) {
	input := `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: built-environment
  version: "1.0.0"
  display_name:
    en: "Built Environment"
    vi: "Môi Trường Xây Dựng"
  description:
    en: "Construction, FM, Smart Buildings"
spec:
  platform_version: ">=1.0.0"
  ontology_files:
    - ontology/core.yaml
    - ontology/brick-compat.yaml
  agent_profiles:
    - agents/fm-operations.yaml
  tools:
    - tools/tier3-tools.yaml
`
	m, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if m.APIVersion != "ontogis.ai/v1" {
		t.Errorf("APIVersion = %q, want %q", m.APIVersion, "ontogis.ai/v1")
	}
	if m.Kind != "DomainKitManifest" {
		t.Errorf("Kind = %q, want %q", m.Kind, "DomainKitManifest")
	}
	if m.Metadata.Name != "built-environment" {
		t.Errorf("Metadata.Name = %q, want %q", m.Metadata.Name, "built-environment")
	}
	if m.Metadata.Version != "1.0.0" {
		t.Errorf("Metadata.Version = %q, want %q", m.Metadata.Version, "1.0.0")
	}
	if len(m.Spec.OntologyFiles) != 2 {
		t.Errorf("OntologyFiles count = %d, want 2", len(m.Spec.OntologyFiles))
	}
	if len(m.Spec.AgentProfiles) != 1 {
		t.Errorf("AgentProfiles count = %d, want 1", len(m.Spec.AgentProfiles))
	}
	if len(m.Spec.Tools) != 1 {
		t.Errorf("Tools count = %d, want 1", len(m.Spec.Tools))
	}
}

func TestValidate_MissingFields(t *testing.T) {
	tests := []struct {
		name    string
		m       KitManifest
		wantErr string
	}{
		{
			name:    "missing api_version",
			m:       KitManifest{Kind: "DomainKitManifest", Metadata: KitMetadata{Name: "x", Version: "1.0.0"}, Spec: KitSpec{PlatformVersion: ">=1.0.0"}},
			wantErr: "api_version is required",
		},
		{
			name:    "missing kind",
			m:       KitManifest{APIVersion: "v1", Metadata: KitMetadata{Name: "x", Version: "1.0.0"}, Spec: KitSpec{PlatformVersion: ">=1.0.0"}},
			wantErr: "kind is required",
		},
		{
			name:    "wrong kind",
			m:       KitManifest{APIVersion: "v1", Kind: "Wrong", Metadata: KitMetadata{Name: "x", Version: "1.0.0"}, Spec: KitSpec{PlatformVersion: ">=1.0.0"}},
			wantErr: "kind must be DomainKitManifest",
		},
		{
			name:    "missing name",
			m:       KitManifest{APIVersion: "v1", Kind: "DomainKitManifest", Metadata: KitMetadata{Version: "1.0.0"}, Spec: KitSpec{PlatformVersion: ">=1.0.0"}},
			wantErr: "metadata.name is required",
		},
		{
			name:    "missing version",
			m:       KitManifest{APIVersion: "v1", Kind: "DomainKitManifest", Metadata: KitMetadata{Name: "x"}, Spec: KitSpec{PlatformVersion: ">=1.0.0"}},
			wantErr: "metadata.version is required",
		},
		{
			name:    "missing platform_version",
			m:       KitManifest{APIVersion: "v1", Kind: "DomainKitManifest", Metadata: KitMetadata{Name: "x", Version: "1.0.0"}, Spec: KitSpec{}},
			wantErr: "spec.platform_version is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(&tt.m)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidate_ValidManifest(t *testing.T) {
	m := &KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata: KitMetadata{
			Name:    "test-kit",
			Version: "1.0.0",
		},
		Spec: KitSpec{
			PlatformVersion: ">=1.0.0",
		},
	}

	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
