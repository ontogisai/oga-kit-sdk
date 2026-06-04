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

// TestParse_PromptFragments verifies the new prompt_fragments field round-trips
// through YAML with both target and file populated for each entry.
func TestParse_PromptFragments(t *testing.T) {
	input := `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: built-environment
  version: "1.0.0"
spec:
  platform_version: ">=1.0.0"
  prompt_fragments:
    - target: frontier
      file: prompt_fragments/frontier_hints.txt
    - target: knowledge
      file: prompt_fragments/knowledge_domain.txt
`
	m, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(m.Spec.PromptFragments); got != 2 {
		t.Fatalf("PromptFragments count = %d, want 2", got)
	}
	if m.Spec.PromptFragments[0].Target != "frontier" {
		t.Errorf("[0].Target = %q, want %q", m.Spec.PromptFragments[0].Target, "frontier")
	}
	if m.Spec.PromptFragments[0].File != "prompt_fragments/frontier_hints.txt" {
		t.Errorf("[0].File = %q", m.Spec.PromptFragments[0].File)
	}
	if m.Spec.PromptFragments[1].Target != "knowledge" {
		t.Errorf("[1].Target = %q, want %q", m.Spec.PromptFragments[1].Target, "knowledge")
	}

	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestParse_RejectsLegacyRendererPrompts proves the clean-break: a manifest
// using the removed renderer_prompts field is rejected because the YAML
// decoder runs in KnownFields(true) mode.
func TestParse_RejectsLegacyRendererPrompts(t *testing.T) {
	input := `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: legacy-kit
  version: "1.0.0"
spec:
  platform_version: ">=1.0.0"
  renderer_prompts:
    - renderer/domain_prompt.txt
`
	if _, err := Parse(strings.NewReader(input)); err == nil {
		t.Fatal("expected Parse to reject legacy renderer_prompts field, got nil error")
	} else if !strings.Contains(err.Error(), "renderer_prompts") {
		t.Errorf("error = %q, want it to mention the unknown field renderer_prompts", err)
	}
}

// TestValidate_PromptFragments_MissingFields covers the per-entry rules:
// every PromptFragmentEntry MUST have both target and file, and target
// MUST be one of the recognized values.
func TestValidate_PromptFragments_MissingFields(t *testing.T) {
	base := KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata:   KitMetadata{Name: "k", Version: "1.0.0"},
		Spec:       KitSpec{PlatformVersion: ">=1.0.0"},
	}

	tests := []struct {
		name    string
		entries []PromptFragmentEntry
		wantErr string
	}{
		{
			name:    "missing target",
			entries: []PromptFragmentEntry{{File: "x.txt"}},
			wantErr: "target is required",
		},
		{
			name:    "missing file",
			entries: []PromptFragmentEntry{{Target: "frontier"}},
			wantErr: "file is required",
		},
		{
			name:    "unknown target",
			entries: []PromptFragmentEntry{{Target: "convergence", File: "x.txt"}},
			wantErr: "not recognized",
		},
		{
			name: "second entry invalid",
			entries: []PromptFragmentEntry{
				{Target: "frontier", File: "f.txt"},
				{Target: "", File: "k.txt"},
			},
			wantErr: "spec.prompt_fragments[1]: target is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base
			m.Spec.PromptFragments = tt.entries
			err := Validate(&m)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestValidate_PromptFragments_AllRecognizedTargets verifies frontier,
// knowledge, and analytics all pass validation.
func TestValidate_PromptFragments_AllRecognizedTargets(t *testing.T) {
	m := &KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata:   KitMetadata{Name: "k", Version: "1.0.0"},
		Spec: KitSpec{
			PlatformVersion: ">=1.0.0",
			PromptFragments: []PromptFragmentEntry{
				{Target: "frontier", File: "a.txt"},
				{Target: "knowledge", File: "b.txt"},
				{Target: "analytics", File: "c.txt"},
			},
		},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestIsValidPromptFragmentTarget exercises the helper directly.
func TestIsValidPromptFragmentTarget(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"frontier", true},
		{"knowledge", true},
		{"analytics", true},
		{"", false},
		{"convergence", false},
		{"FRONTIER", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsValidPromptFragmentTarget(tt.in); got != tt.want {
			t.Errorf("IsValidPromptFragmentTarget(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
