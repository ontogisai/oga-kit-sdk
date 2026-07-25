package manifest

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Tool-definitions (tier3-tools.yaml) parsing + validation.
//
// A kit's MCP tool server sidecar advertises its tools through a tool-
// definitions file listed under manifest spec.tools[]. The platform parses
// this file at install time to (a) deploy the sidecar declared in spec.sidecar
// and (b) register spec.tools[] in the MCP catalog. This SDK type + validator
// mirrors the platform's internal/domainkit tool validation field-for-field so
// a kit author gets the SAME field-level error locally (in `go test`) that the
// platform would raise at install/upload time — closing the class of bugs where
// a malformed tool schema only surfaces when a customer uploads the bundle
// (OGA-582). It is the tool-file peer of manifest.Validate.

// maxToolNameLength is the maximum allowed length for a tool name. Mirrors the
// platform's internal/domainkit.maxToolNameLength.
const maxToolNameLength = 64

// ToolDefinitionsKind is the required `kind` of a tool-definitions file.
const ToolDefinitionsKind = "MCPToolDefinitions"

// ToolDefinitionsFile is the top-level structure of a tier3-tools.yaml file.
// Mirrors the platform's internal/domainkit.ToolDefinitionsFile.
type ToolDefinitionsFile struct {
	APIVersion string                  `yaml:"api_version" json:"api_version"`
	Kind       string                  `yaml:"kind" json:"kind"`
	Metadata   ToolDefinitionsMetadata `yaml:"metadata" json:"metadata"`
	Spec       ToolDefinitionsSpec     `yaml:"spec" json:"spec"`
}

// ToolDefinitionsMetadata contains metadata about the tool-definitions file.
type ToolDefinitionsMetadata struct {
	Name        string            `yaml:"name" json:"name"`
	DisplayName map[string]string `yaml:"display_name" json:"display_name,omitempty"`
	Description map[string]string `yaml:"description" json:"description,omitempty"`
	Tier        int               `yaml:"tier" json:"tier,omitempty"`
	Domain      string            `yaml:"domain" json:"domain,omitempty"`
}

// ToolDefinitionsSpec is the spec block: the sidecar that hosts the tools plus
// the tool list itself.
type ToolDefinitionsSpec struct {
	Sidecar SidecarContainerSpec  `yaml:"sidecar,omitempty" json:"sidecar,omitempty"`
	Tools   []*ToolDefinitionYAML `yaml:"tools" json:"tools"`
}

// ToolDefinitionYAML is a single MCP tool definition. Mirrors the platform's
// internal/domainkit.ToolDefinitionYAML.
type ToolDefinitionYAML struct {
	// Name is the tool's unique identifier (<= 64 chars, unique within the file).
	Name string `yaml:"name" json:"name"`

	// DisplayName is the human-readable tool name, keyed by full BCP-47 locale
	// (i18n — legitimately localizable, like the manifest's display fields).
	DisplayName map[string]string `yaml:"display_name,omitempty" json:"display_name,omitempty"`

	// Description is the tool-level description, keyed by full BCP-47 locale
	// (i18n — legitimately localizable). NOTE: this is DISTINCT from a JSON
	// Schema property `description` inside InputSchema, which MUST be a plain
	// string (see ValidateToolInputSchema).
	Description map[string]string `yaml:"description" json:"description"`

	// InputSchema is the JSON Schema for the tool's arguments.
	InputSchema map[string]any `yaml:"input_schema" json:"input_schema"`

	// Mutates marks a state-changing tool (confirm-before-write gate).
	Mutates bool `yaml:"mutates,omitempty" json:"mutates,omitempty"`

	// CompositionReference records the platform Tier 1/2 tools the sidecar
	// handler calls internally. Documentation only — the platform does not read
	// it; carried here so a strict author-side round-trip does not lose it.
	CompositionReference []string `yaml:"composition_reference,omitempty" json:"composition_reference,omitempty"`
}

// ParseToolDefinitions parses and validates a tier3-tools.yaml document. It
// enforces the SAME rules as the platform's internal/domainkit.ParseToolDefinitions:
//   - kind must be "MCPToolDefinitions"
//   - tool names are non-empty, unique, and <= 64 characters
//   - each tool has a non-empty description in at least one locale
//   - input_schema is structurally valid JSON Schema (has a valid "type"), and
//     every `description` within it (recursively) is a plain string, never a
//     localized object (OGA-582)
//
// An empty tool list is valid (returns nil, nil). Returns the parsed tools so a
// caller can do further checks (e.g. schema/runtime parity).
func ParseToolDefinitions(data []byte) ([]*ToolDefinitionYAML, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("tool definitions data is empty")
	}

	var file ToolDefinitionsFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("YAML parse error: %w", err)
	}

	if file.Kind != ToolDefinitionsKind {
		return nil, fmt.Errorf("unsupported kind %q, expected %q", file.Kind, ToolDefinitionsKind)
	}

	tools := file.Spec.Tools
	if len(tools) == 0 {
		return nil, nil // empty tool list is valid (no-op)
	}

	seen := make(map[string]bool, len(tools))
	for i, tool := range tools {
		if tool.Name == "" {
			return nil, fmt.Errorf("tool at index %d has empty name", i)
		}
		if len(tool.Name) > maxToolNameLength {
			return nil, fmt.Errorf("tool name %q exceeds maximum length of %d characters", tool.Name, maxToolNameLength)
		}
		if seen[tool.Name] {
			return nil, fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true

		if len(tool.Description) == 0 {
			return nil, fmt.Errorf("tool %q has empty description", tool.Name)
		}
		hasNonEmpty := false
		for _, desc := range tool.Description {
			if desc != "" {
				hasNonEmpty = true
				break
			}
		}
		if !hasNonEmpty {
			return nil, fmt.Errorf("tool %q has no non-empty description in any locale", tool.Name)
		}

		if err := ValidateToolInputSchema(tool.Name, tool.InputSchema); err != nil {
			return nil, err
		}
	}

	return tools, nil
}

// ParseToolDefinitionsFile reads and validates a tier3-tools.yaml from a path.
func ParseToolDefinitionsFile(path string) ([]*ToolDefinitionYAML, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tool definitions %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read tool definitions %s: %w", path, err)
	}
	return ParseToolDefinitions(data)
}

// ValidateToolInputSchema performs structural validation of a tool's JSON
// Schema: it requires a valid "type", requires "properties" (when present) to
// be an object, and requires every "description" (at any depth) to be a plain
// string. Mirrors the platform's internal/domainkit.validateInputSchema.
func ValidateToolInputSchema(toolName string, schema map[string]any) error {
	if schema == nil {
		return fmt.Errorf("tool %q has nil input_schema", toolName)
	}

	typeVal, ok := schema["type"]
	if !ok {
		return fmt.Errorf("tool %q input_schema missing required 'type' field", toolName)
	}
	typeStr, ok := typeVal.(string)
	if !ok {
		return fmt.Errorf("tool %q input_schema 'type' must be a string", toolName)
	}
	validTypes := map[string]bool{
		"object": true, "array": true, "string": true,
		"number": true, "integer": true, "boolean": true, "null": true,
	}
	if !validTypes[typeStr] {
		return fmt.Errorf("tool %q input_schema has invalid type %q", toolName, typeStr)
	}
	if typeStr == "object" {
		if props, exists := schema["properties"]; exists {
			if _, ok := props.(map[string]any); !ok {
				return fmt.Errorf("tool %q input_schema 'properties' must be an object", toolName)
			}
		}
	}

	// A property-level `description` declared as a localized object
	// (`{ en-US: "..." }`) unmarshals cleanly as YAML but is rejected by the
	// platform's JSON Schema decode target (its Description field is a string),
	// silently dropping the sidecar's entire tool set at catalog registration
	// (OGA-582). JSON Schema property descriptions are NOT localizable — reject
	// a non-string description here, at author time.
	return validateToolSchemaDescriptions(toolName, "input_schema", schema)
}

// validateToolSchemaDescriptions recursively verifies that every `description`
// field in a JSON Schema node is a string, descending into `properties` and
// `items` (single-schema and tuple forms). Mirrors the platform's
// internal/domainkit.validateSchemaDescriptions.
func validateToolSchemaDescriptions(toolName, path string, node map[string]any) error {
	if desc, ok := node["description"]; ok {
		if _, isString := desc.(string); !isString {
			return fmt.Errorf(
				"tool %q %s.description must be a string, not %T — JSON Schema property descriptions are not localizable; use a plain string",
				toolName, path, desc)
		}
	}

	if props, ok := node["properties"].(map[string]any); ok {
		for name, sub := range props {
			if subSchema, ok := sub.(map[string]any); ok {
				if err := validateToolSchemaDescriptions(toolName, path+".properties."+name, subSchema); err != nil {
					return err
				}
			}
		}
	}

	switch items := node["items"].(type) {
	case map[string]any:
		if err := validateToolSchemaDescriptions(toolName, path+".items", items); err != nil {
			return err
		}
	case []any:
		for i, it := range items {
			if itSchema, ok := it.(map[string]any); ok {
				if err := validateToolSchemaDescriptions(toolName, fmt.Sprintf("%s.items[%d]", path, i), itSchema); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
