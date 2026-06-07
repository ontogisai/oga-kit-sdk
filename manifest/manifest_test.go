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
    en-US: "Built Environment"
    vi-VN: "Môi Trường Xây Dựng"
  description:
    en-US: "Construction, FM, Smart Buildings"
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
	if got := m.Metadata.DisplayName["en-US"]; got != "Built Environment" {
		t.Errorf("display_name[en-US] = %q, want %q", got, "Built Environment")
	}
	if got := m.Metadata.DisplayName["vi-VN"]; got != "Môi Trường Xây Dựng" {
		t.Errorf("display_name[vi-VN] = %q", got)
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

	// Validate must accept the full-form locale keys.
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
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

// TestValidate_RejectsShortFormLocaleKeys verifies the OGA-51 contract:
// kit manifests MUST use full BCP-47 locale tags (en-US, vi-VN). Short
// forms (en, vi) are silently routed by the platform's matcher at
// runtime, but a manifest is a declarative artifact — kit authors must
// spell out the region so intent is unambiguous.
func TestValidate_RejectsShortFormLocaleKeys(t *testing.T) {
	tests := []struct {
		name  string
		field string
		m     *KitManifest
	}{
		{
			name:  "display_name short en",
			field: "metadata.display_name",
			m: &KitManifest{
				APIVersion: "ontogis.ai/v1",
				Kind:       "DomainKitManifest",
				Metadata: KitMetadata{
					Name:        "k",
					Version:     "1.0.0",
					DisplayName: map[string]string{"en": "Kit"},
				},
				Spec: KitSpec{PlatformVersion: ">=1.0.0"},
			},
		},
		{
			name:  "display_name short vi",
			field: "metadata.display_name",
			m: &KitManifest{
				APIVersion: "ontogis.ai/v1",
				Kind:       "DomainKitManifest",
				Metadata: KitMetadata{
					Name:        "k",
					Version:     "1.0.0",
					DisplayName: map[string]string{"vi": "Bộ Khởi Động"},
				},
				Spec: KitSpec{PlatformVersion: ">=1.0.0"},
			},
		},
		{
			name:  "description short zh",
			field: "metadata.description",
			m: &KitManifest{
				APIVersion: "ontogis.ai/v1",
				Kind:       "DomainKitManifest",
				Metadata: KitMetadata{
					Name:        "k",
					Version:     "1.0.0",
					Description: map[string]string{"zh": "套件"},
				},
				Spec: KitSpec{PlatformVersion: ">=1.0.0"},
			},
		},
		{
			name:  "mixed short and full",
			field: "metadata.display_name",
			m: &KitManifest{
				APIVersion: "ontogis.ai/v1",
				Kind:       "DomainKitManifest",
				Metadata: KitMetadata{
					Name:    "k",
					Version: "1.0.0",
					DisplayName: map[string]string{
						"en-US": "Kit",
						"vi":    "Bộ Khởi Động", // short — should fail
					},
				},
				Spec: KitSpec{PlatformVersion: ">=1.0.0"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.m)
			if err == nil {
				t.Fatal("expected error for short-form locale key, got nil")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name the offending field %q", err, tt.field)
			}
			if !strings.Contains(err.Error(), "short-form") {
				t.Errorf("error %q does not mention short-form rejection", err)
			}
		})
	}
}

// TestValidate_AcceptsFullBCP47 covers the happy path: full-form locale
// keys (with explicit region or script-region) pass validation across
// the manifest's locale-keyed fields.
func TestValidate_AcceptsFullBCP47(t *testing.T) {
	m := &KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata: KitMetadata{
			Name:    "k",
			Version: "1.0.0",
			DisplayName: map[string]string{
				"en-US": "Kit",
				"vi-VN": "Bộ Khởi Động",
				"zh-CN": "套件",
			},
			Description: map[string]string{
				"en-US": "A test kit",
				"vi-VN": "Bộ thử nghiệm",
			},
		},
		Spec: KitSpec{PlatformVersion: ">=1.0.0"},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidate_RejectsMalformedLocaleKeys covers values that aren't
// even valid BCP-47.
func TestValidate_RejectsMalformedLocaleKeys(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"empty key", ""},
		{"contains underscore", "en_US"},
		{"trailing dash", "en-"},
		{"random garbage", "!!!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &KitManifest{
				APIVersion: "ontogis.ai/v1",
				Kind:       "DomainKitManifest",
				Metadata: KitMetadata{
					Name:        "k",
					Version:     "1.0.0",
					DisplayName: map[string]string{tt.key: "Kit"},
				},
				Spec: KitSpec{PlatformVersion: ">=1.0.0"},
			}
			err := Validate(m)
			if err == nil {
				t.Fatal("expected error for malformed locale key, got nil")
			}
			if !strings.Contains(err.Error(), "metadata.display_name") {
				t.Errorf("error should name the field, got: %v", err)
			}
		})
	}
}

// TestValidateLocaleKeys_PublicHelper exercises the exported helper
// the transfer package re-exports for kit-author use.
func TestValidateLocaleKeys_PublicHelper(t *testing.T) {
	t.Run("nil map ok", func(t *testing.T) {
		if err := ValidateLocaleKeys("test.field", nil); err != nil {
			t.Errorf("nil map should pass, got %v", err)
		}
	})
	t.Run("empty map ok", func(t *testing.T) {
		if err := ValidateLocaleKeys("test.field", map[string]string{}); err != nil {
			t.Errorf("empty map should pass, got %v", err)
		}
	})
	t.Run("full BCP-47 ok", func(t *testing.T) {
		if err := ValidateLocaleKeys("test.field", map[string]string{
			"en-US": "x", "vi-VN": "y",
		}); err != nil {
			t.Errorf("full BCP-47 should pass, got %v", err)
		}
	})
	t.Run("short rejected with field name", func(t *testing.T) {
		err := ValidateLocaleKeys("ontology.entity_type.display_name", map[string]string{"en": "x"})
		if err == nil {
			t.Fatal("short-form should fail")
		}
		if !strings.Contains(err.Error(), "ontology.entity_type.display_name") {
			t.Errorf("error %q should mention the field name", err)
		}
	})
}

// TestParse_KitPolicies covers the happy path: a manifest carrying a valid
// spec.policies block with both routing-level and data-level entries
// parses cleanly, the values round-trip into the typed structure, and
// Validate accepts them.
func TestParse_KitPolicies(t *testing.T) {
	input := `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: built-environment
  version: "1.0.0"
spec:
  platform_version: ">=1.0.0"
  policies:
    - id_suffix: fm-operator-allow-fm-operations
      level: routing
      name: "FM operator can invoke FM operations agent"
      description: "Routing-level gate for FM operators."
      target_roles: [fm_operator, tenant_admin]
      target_agent_ids: [fm-operations-agent]
      expression: '"fm_operator" in principal_roles'
      priority: 200
    - id_suffix: fm-write-restricted-to-operator
      level: data
      name: "Only FM operators write WorkOrder"
      target_entity_types: [WorkOrder, MaintenanceSchedule]
      target_actions: [write, delete]
      expression: '"fm_operator" in principal_roles'
      priority: 150
`
	m, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(m.Spec.Policies); got != 2 {
		t.Fatalf("Policies count = %d, want 2", got)
	}
	p0 := m.Spec.Policies[0]
	if p0.IDSuffix != "fm-operator-allow-fm-operations" {
		t.Errorf("[0].IDSuffix = %q", p0.IDSuffix)
	}
	if p0.Level != PolicyLevelRouting {
		t.Errorf("[0].Level = %q, want %q", p0.Level, PolicyLevelRouting)
	}
	if p0.Priority != 200 {
		t.Errorf("[0].Priority = %d, want 200", p0.Priority)
	}
	if len(p0.TargetRoles) != 2 || p0.TargetRoles[0] != "fm_operator" {
		t.Errorf("[0].TargetRoles = %v", p0.TargetRoles)
	}
	if len(p0.TargetAgentIDs) != 1 || p0.TargetAgentIDs[0] != "fm-operations-agent" {
		t.Errorf("[0].TargetAgentIDs = %v", p0.TargetAgentIDs)
	}

	p1 := m.Spec.Policies[1]
	if p1.Level != PolicyLevelData {
		t.Errorf("[1].Level = %q, want %q", p1.Level, PolicyLevelData)
	}
	if len(p1.TargetEntityTypes) != 2 {
		t.Errorf("[1].TargetEntityTypes count = %d", len(p1.TargetEntityTypes))
	}
	if len(p1.TargetActions) != 2 || p1.TargetActions[0] != "write" {
		t.Errorf("[1].TargetActions = %v", p1.TargetActions)
	}

	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidate_KitPolicies_MissingFields covers the per-entry rules:
// every KitPolicySpec MUST have id_suffix, level, name, expression. The
// id_suffix MUST match the platform's regex, and id_suffix values must be
// unique within the kit.
//
// Error messages are checked verbatim against the platform's
// internal/domainkit.validateKitPolicies wording so kit authors get
// identical feedback whether they fail SDK Validate locally or fail at
// install time on a tenant's platform.
func TestValidate_KitPolicies_MissingFields(t *testing.T) {
	base := KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata:   KitMetadata{Name: "k", Version: "1.0.0"},
		Spec:       KitSpec{PlatformVersion: ">=1.0.0"},
	}

	tests := []struct {
		name     string
		policies []KitPolicySpec
		wantErr  string
	}{
		{
			name: "missing id_suffix",
			policies: []KitPolicySpec{
				{Level: "routing", Name: "x", Expression: "true"},
			},
			wantErr: "spec.policies[0]: id_suffix is required",
		},
		{
			name: "id_suffix too short",
			policies: []KitPolicySpec{
				{IDSuffix: "ab", Level: "routing", Name: "x", Expression: "true"},
			},
			wantErr: `id_suffix = "ab" does not match`,
		},
		{
			name: "id_suffix has uppercase",
			policies: []KitPolicySpec{
				{IDSuffix: "FM-Operator", Level: "routing", Name: "x", Expression: "true"},
			},
			wantErr: `id_suffix = "FM-Operator" does not match`,
		},
		{
			name: "id_suffix starts with digit",
			policies: []KitPolicySpec{
				{IDSuffix: "1-leading-digit", Level: "routing", Name: "x", Expression: "true"},
			},
			wantErr: `id_suffix = "1-leading-digit" does not match`,
		},
		{
			name: "id_suffix ends with hyphen",
			policies: []KitPolicySpec{
				{IDSuffix: "trailing-hyphen-", Level: "routing", Name: "x", Expression: "true"},
			},
			wantErr: `id_suffix = "trailing-hyphen-" does not match`,
		},
		{
			name: "missing level",
			policies: []KitPolicySpec{
				{IDSuffix: "valid-id", Name: "x", Expression: "true"},
			},
			wantErr: "spec.policies[0]: level is required",
		},
		{
			name: "invalid level",
			policies: []KitPolicySpec{
				{IDSuffix: "valid-id", Level: "advisory", Name: "x", Expression: "true"},
			},
			wantErr: `level = "advisory" is invalid`,
		},
		{
			name: "missing name",
			policies: []KitPolicySpec{
				{IDSuffix: "valid-id", Level: "data", Expression: "true"},
			},
			wantErr: "spec.policies[0]: name is required",
		},
		{
			name: "missing expression",
			policies: []KitPolicySpec{
				{IDSuffix: "valid-id", Level: "data", Name: "x"},
			},
			wantErr: "spec.policies[0]: expression is required",
		},
		{
			name: "duplicate id_suffix",
			policies: []KitPolicySpec{
				{IDSuffix: "duplicate", Level: "routing", Name: "first", Expression: "true"},
				{IDSuffix: "duplicate", Level: "data", Name: "second", Expression: "true"},
			},
			wantErr: `spec.policies[1]: id_suffix = "duplicate" duplicates spec.policies[0]`,
		},
		{
			name: "second entry invalid (positional error)",
			policies: []KitPolicySpec{
				{IDSuffix: "first-ok", Level: "routing", Name: "first", Expression: "true"},
				{IDSuffix: "second", Level: "", Name: "second", Expression: "true"},
			},
			wantErr: "spec.policies[1]: level is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base
			m.Spec.Policies = tt.policies
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

// TestValidate_KitPolicies_AcceptsBothLevels verifies that policies with
// either routing or data level pass validation.
func TestValidate_KitPolicies_AcceptsBothLevels(t *testing.T) {
	m := &KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata:   KitMetadata{Name: "k", Version: "1.0.0"},
		Spec: KitSpec{
			PlatformVersion: ">=1.0.0",
			Policies: []KitPolicySpec{
				{
					IDSuffix:   "routing-policy",
					Level:      PolicyLevelRouting,
					Name:       "routing example",
					Expression: `"role" in principal_roles`,
					Priority:   200,
				},
				{
					IDSuffix:   "data-policy",
					Level:      PolicyLevelData,
					Name:       "data example",
					Expression: `resource_classification != "secret"`,
					Priority:   100,
				},
			},
		},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidate_KitPolicies_EmptyIsValid confirms the absence of a policies
// block is not an error — kits don't have to declare PBAC policies.
func TestValidate_KitPolicies_EmptyIsValid(t *testing.T) {
	m := &KitManifest{
		APIVersion: "ontogis.ai/v1",
		Kind:       "DomainKitManifest",
		Metadata:   KitMetadata{Name: "k", Version: "1.0.0"},
		Spec:       KitSpec{PlatformVersion: ">=1.0.0"},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestIsValidPolicyLevel exercises the helper directly.
func TestIsValidPolicyLevel(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"routing", true},
		{"data", true},
		{"", false},
		{"advisory", false},
		{"Routing", false}, // case-sensitive
		{" routing", false},
	}
	for _, tt := range tests {
		if got := IsValidPolicyLevel(tt.in); got != tt.want {
			t.Errorf("IsValidPolicyLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
