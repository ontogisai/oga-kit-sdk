package manifest

import (
	"fmt"
	"io"
	"os"

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

	// DisplayName is the human-readable name, keyed by locale.
	DisplayName map[string]string `yaml:"display_name"`

	// Description is a detailed description, keyed by locale.
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

	// RendererPrompts lists paths to LLM renderer prompt files.
	RendererPrompts []string `yaml:"renderer_prompts,omitempty"`

	// Extensibility defines namespace policies for extension kits.
	Extensibility *Extensibility `yaml:"extensibility,omitempty"`

	// Workflows lists workflow configurations.
	Workflows []WorkflowConfig `yaml:"workflows,omitempty"`
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
	if m.Spec.PlatformVersion == "" {
		return fmt.Errorf("spec.platform_version is required")
	}
	return nil
}
