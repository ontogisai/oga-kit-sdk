package manifest

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

// KitManifest is the top-level structure of a domain kit manifest.yaml file.
type KitManifest struct {
	APIVersion string      `yaml:"api_version"`
	Kind       string      `yaml:"kind"`
	Metadata   KitMetadata `yaml:"metadata"`
	Spec       KitSpec     `yaml:"spec"`
}

// KitMetadata contains identifying information about the kit.
type KitMetadata struct {
	// Name is the kit identifier (e.g., "built-environment", "anycompany-maintenance").
	Name string `yaml:"name"`

	// Version is the semantic version of this kit release.
	Version string `yaml:"version"`

	// DisplayName is the human-readable name, keyed by full BCP-47 locale
	// tag (e.g., "en-US", "vi-VN"). Short-form keys ("en", "vi") are
	// rejected by Validate so kit authors can never silently disagree
	// with the platform's locale parsing. The convention is enforced
	// in OGA-51.
	DisplayName map[string]string `yaml:"display_name"`

	// Description is a detailed description, keyed by full BCP-47 locale
	// tag — same convention as DisplayName.
	Description map[string]string `yaml:"description"`

	// Authors lists the kit authors.
	Authors []Author `yaml:"authors,omitempty"`
}

// Author identifies a kit author.
type Author struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email,omitempty"`
}

// KitSpec defines the kit's contents and configuration.
type KitSpec struct {
	// PlatformVersion is a semver constraint for platform compatibility.
	PlatformVersion string `yaml:"platform_version"`

	// Dependencies lists other kits that must be installed first.
	Dependencies []Dependency `yaml:"dependencies,omitempty"`

	// OntologyFiles lists paths to ontology YAML files (entity + relationship types).
	OntologyFiles []string `yaml:"ontology_files,omitempty"`

	// ExtensionFiles lists paths to regional extension YAML files.
	ExtensionFiles []string `yaml:"extension_files,omitempty"`

	// AgentProfiles lists paths to agent profile YAML files.
	AgentProfiles []string `yaml:"agent_profiles,omitempty"`

	// Tools lists paths to MCP tool definition YAML files.
	Tools []string `yaml:"tools,omitempty"`

	// PrivacyAnnotations lists paths to privacy annotation files.
	PrivacyAnnotations []string `yaml:"privacy_annotations,omitempty"`

	// SampleData lists paths to sample data files.
	SampleData []string `yaml:"sample_data,omitempty"`

	// IngestionTemplates lists paths to ingestion template definitions.
	IngestionTemplates []string `yaml:"ingestion_templates,omitempty"`

	// GroundingDocuments lists grounding document definitions.
	GroundingDocuments []GroundingDocument `yaml:"grounding_documents,omitempty"`

	// Translations lists paths to i18n translation bundle files.
	Translations []string `yaml:"translations,omitempty"`

	// PromptFragments lists per-tenant LLM prompt fragments that augment
	// platform built-in agents (Frontier, Knowledge). Each entry MUST specify
	// both the target agent and the file path. The platform composes the
	// agent's system prompt from a neutral base + these fragments + dynamic
	// runtime context — see oga-platform spec
	// platform-agent-prompt-composition for the full design.
	PromptFragments []PromptFragmentEntry `yaml:"prompt_fragments,omitempty"`

	// Extensibility defines namespace policies for extension kits.
	Extensibility *Extensibility `yaml:"extensibility,omitempty"`

	// Workflows lists workflow configurations.
	Workflows []WorkflowConfig `yaml:"workflows,omitempty"`
}

// PromptFragmentEntry declares a prompt fragment file targeting a specific
// platform built-in agent. Both fields are required.
//
// Recognized targets:
//   - "frontier"  — augments the Frontier intent-classification + system prompt
//   - "knowledge" — augments the Knowledge tool planner + assembly prompt
//   - "analytics" — reserved for kit-supplied analytics agents (the platform
//     ships no built-in for this target)
type PromptFragmentEntry struct {
	// Target identifies the built-in agent the fragment augments.
	Target string `yaml:"target"`

	// File is the path (relative to the kit root) of the fragment text file.
	File string `yaml:"file"`
}

// PromptFragmentTarget enumerates the recognized values for
// PromptFragmentEntry.Target.
type PromptFragmentTarget = string

// Recognized prompt fragment targets.
const (
	PromptFragmentTargetFrontier  PromptFragmentTarget = "frontier"
	PromptFragmentTargetKnowledge PromptFragmentTarget = "knowledge"
	PromptFragmentTargetAnalytics PromptFragmentTarget = "analytics"
)

// IsValidPromptFragmentTarget reports whether t is one of the recognized
// prompt fragment target values.
func IsValidPromptFragmentTarget(t string) bool {
	switch t {
	case PromptFragmentTargetFrontier,
		PromptFragmentTargetKnowledge,
		PromptFragmentTargetAnalytics:
		return true
	default:
		return false
	}
}

// Dependency declares a required kit dependency.
type Dependency struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// GroundingDocument defines a document to be ingested for agent grounding.
type GroundingDocument struct {
	Path     string                 `yaml:"path"`
	Metadata map[string]interface{} `yaml:"metadata,omitempty"`
}

// Extensibility defines namespace policies for extension kits.
type Extensibility struct {
	AllowCustomEntityTypes   bool      `yaml:"allow_custom_entity_types"`
	AllowCustomRelationships bool      `yaml:"allow_custom_relationships"`
	AllowCustomProperties    bool      `yaml:"allow_custom_properties"`
	ExtensionNamespacePrefix string    `yaml:"extension_namespace_prefix"`
	ProtectedNamespaces      []string  `yaml:"protected_namespaces,omitempty"`
	NamespacePolicy          *NSPolicy `yaml:"namespace_policy,omitempty"`
}

// NSPolicy defines namespace access levels.
type NSPolicy struct {
	Locked     []string `yaml:"locked,omitempty"`
	Extendable []string `yaml:"extendable,omitempty"`
	Open       []string `yaml:"open,omitempty"`
}

// WorkflowConfig defines a workflow configuration for the kit.
type WorkflowConfig struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Config map[string]any `yaml:"config,omitempty"`
}

// Parse reads and parses a manifest from the given reader.
func Parse(r io.Reader) (*KitManifest, error) {
	var m KitManifest
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// ParseFile reads and parses a manifest from the given file path.
func ParseFile(path string) (*KitManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}

// Validate checks the manifest for structural correctness.
func Validate(m *KitManifest) error {
	if m.APIVersion == "" {
		return fmt.Errorf("api_version is required")
	}
	if m.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if m.Kind != "DomainKitManifest" {
		return fmt.Errorf("kind must be DomainKitManifest, got %q", m.Kind)
	}
	if m.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if m.Metadata.Version == "" {
		return fmt.Errorf("metadata.version is required")
	}
	if err := validateLocaleKeys("metadata.display_name", m.Metadata.DisplayName); err != nil {
		return err
	}
	if err := validateLocaleKeys("metadata.description", m.Metadata.Description); err != nil {
		return err
	}
	if m.Spec.PlatformVersion == "" {
		return fmt.Errorf("spec.platform_version is required")
	}
	for i, e := range m.Spec.PromptFragments {
		if e.Target == "" {
			return fmt.Errorf("spec.prompt_fragments[%d]: target is required", i)
		}
		if !IsValidPromptFragmentTarget(e.Target) {
			return fmt.Errorf(
				"spec.prompt_fragments[%d]: target %q is not recognized "+
					"(valid: frontier, knowledge, analytics)",
				i, e.Target,
			)
		}
		if e.File == "" {
			return fmt.Errorf("spec.prompt_fragments[%d]: file is required", i)
		}
	}
	return nil
}

// ValidateLocaleKeys reports whether every key in m is a valid full
// BCP-47 language tag (e.g., "en-US", "vi-VN", "zh-CN"). Short-form
// language-only tags ("en", "vi") are rejected — kit manifests must
// use the full form so the platform's locale parser cannot silently
// disagree on which tag the kit means.
//
// The fieldName argument prefixes any error returned so the kit
// author can find the offending map quickly. An empty or nil map is
// always valid.
//
// This is the kit-facing entry point for validating any locale-keyed
// map a kit author maintains (display names on entity types, property
// descriptions, etc.). The transfer package re-exports the same
// helper as transfer.ValidateLocaleKeys so kit code that does not
// import manifest still has access.
func ValidateLocaleKeys(fieldName string, m map[string]string) error {
	return validateLocaleKeys(fieldName, m)
}

// validateLocaleKeys is the package-internal implementation behind
// the public ValidateLocaleKeys. Iteration order is sorted so error
// messages are stable across map random ordering.
func validateLocaleKeys(fieldName string, m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" {
			return fmt.Errorf(
				"%s: locale key is empty (use full BCP-47 like \"en-US\", \"vi-VN\")",
				fieldName,
			)
		}
		if _, err := language.Parse(k); err != nil {
			return fmt.Errorf(
				"%s: locale key %q is not a valid BCP-47 tag: %w",
				fieldName, k, err,
			)
		}
		// Reject short-form language-only tags. golang.org/x/text/language
		// will happily *infer* a likely region for "en" / "vi" with Low
		// confidence — that's useful at runtime but unsafe for a
		// declarative manifest. Kit authors must spell out the region
		// (en-US vs en-GB; vi-VN vs zh-Hant-VN; etc.) so the kit's
		// intent is unambiguous when the supported-locale list grows.
		// Reject anything missing a "-" since every full BCP-47 tag has
		// at least one subtag separator (language-region or
		// language-script-region). Anything more nuanced is overkill —
		// the platform also enforces the same constraint at lookup time.
		if !strings.Contains(k, "-") {
			return fmt.Errorf(
				"%s: locale key %q must be a full BCP-47 tag with a region "+
					"(e.g., %q-US, %q-GB) — short-form language-only tags are rejected",
				fieldName, k, k, k,
			)
		}
	}
	return nil
}
