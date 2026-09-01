package mcptools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	sdkmanifest "github.com/ontogisai/oga-kit-sdk/manifest"
)

// manifestAPIVersion is the api_version stamped on a rendered tool-definitions
// file (the platform's current kit schema version).
const manifestAPIVersion = "ontogis.ai/v1"

// Kit tool-manifest emission (OGA-582).
//
// A kit declares each Tier 3 tool ONCE as an mcptools.Tool (metadata + JSON
// schema + handler). The runtime serves it via tools/list; RenderToolsManifest
// renders the SAME set into tools/*.yaml — the file the platform parses at
// install to deploy the sidecar and register the tools in the MCP catalog.
// Because the schema is a Go value marshaled here (never hand-authored YAML),
// the two can never diverge and a localized-object property description (the
// OGA-582 bug) is structurally impossible. This replaces the per-kit
// tooldefs-package + generator + bespoke renderer with one SDK-owned emitter
// every kit reuses.

// ManifestMeta is the manifest-level (non-per-tool) configuration a kit
// supplies to render its tools/*.yaml: the file metadata and the sidecar the
// tools are hosted by. Per-tool metadata lives on each Tool.
type ManifestMeta struct {
	// Name is metadata.name — the tool-definitions file name, which is also the
	// sidecar registration name the platform catalog keys on
	// (e.g. "built-environment-tier3-tools").
	Name string
	// DisplayName / Description are the localized file-level display strings.
	DisplayName map[string]string
	Description map[string]string
	// Version is metadata.version. Kits typically pass a release-time
	// placeholder (e.g. "0.0.0-dev") that the release pipeline pins.
	Version string
	// Tier is metadata.tier (3 for domain sidecar tools).
	Tier int
	// Domain is metadata.domain (e.g. "built-environment").
	Domain string
	// Sidecar describes the MCP server container that hosts the tools.
	Sidecar SidecarMeta
	// HeaderComment is an optional comment block prepended to the rendered YAML
	// (e.g. a "DO NOT EDIT — generated" banner). A trailing newline is added if
	// missing. When empty, no header is written.
	HeaderComment string
}

// SidecarMeta describes the MCP server sidecar container in the manifest's
// spec.sidecar block.
type SidecarMeta struct {
	Name     string
	Image    string
	Port     int
	MemoryMB int
	CPUMilli int
}

// --- render structs (control the exact YAML shape + field order) ---

type manifestDoc struct {
	APIVersion string        `yaml:"api_version"`
	Kind       string        `yaml:"kind"`
	Metadata   metadataBlock `yaml:"metadata"`
	Spec       specBlock     `yaml:"spec"`
}

type metadataBlock struct {
	Name        string            `yaml:"name"`
	DisplayName map[string]string `yaml:"display_name,omitempty"`
	Description map[string]string `yaml:"description,omitempty"`
	Version     string            `yaml:"version,omitempty"`
	Tier        int               `yaml:"tier,omitempty"`
	Domain      string            `yaml:"domain,omitempty"`
}

type specBlock struct {
	Sidecar sidecarBlock `yaml:"sidecar"`
	Tools   []toolBlock  `yaml:"tools"`
}

type sidecarBlock struct {
	Name      string        `yaml:"name"`
	Image     string        `yaml:"image"`
	Port      int           `yaml:"port"`
	Resources resourceBlock `yaml:"resources"`
}

type resourceBlock struct {
	MemoryMB int `yaml:"memory_mb"`
	CPUMilli int `yaml:"cpu_milli"`
}

type toolBlock struct {
	Name                 string            `yaml:"name"`
	DisplayName          map[string]string `yaml:"display_name,omitempty"`
	Mutates              bool              `yaml:"mutates,omitempty"`
	Description          map[string]string `yaml:"description"`
	InputSchema          *yaml.Node        `yaml:"input_schema"`
	CompositionReference []string          `yaml:"composition_reference,omitempty"`
}

// RenderToolsManifest renders a kit's tools/*.yaml from its tool definitions and
// manifest metadata. Output is deterministic (stable field + key order) so it
// can be committed and drift-checked. Every tool's InputSchema is validated
// (via manifest.ValidateToolInputSchema — the SAME check the platform runs at
// install/upload), so a malformed schema fails here, at emit/`go generate` time.
func RenderToolsManifest(tools []Tool, meta ManifestMeta) ([]byte, error) {
	if meta.Name == "" {
		return nil, fmt.Errorf("manifest meta: name is required")
	}
	blocks := make([]toolBlock, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("tool has empty name")
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("duplicate tool name %q", t.Name)
		}
		seen[t.Name] = true

		// Validate the schema up front — a non-string property description
		// (OGA-582) or any structural error fails the render, not a later upload.
		var schemaMap map[string]any
		if err := json.Unmarshal(t.InputSchema, &schemaMap); err != nil {
			return nil, fmt.Errorf("tool %q: input_schema is not valid JSON: %w", t.Name, err)
		}
		if err := sdkmanifest.ValidateToolInputSchema(t.Name, schemaMap); err != nil {
			return nil, err
		}

		node, err := schemaToNode(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}
		blocks = append(blocks, toolBlock{
			Name:                 t.Name,
			DisplayName:          t.DisplayNameI18n,
			Mutates:              t.Mutates,
			Description:          manifestDescriptions(t),
			InputSchema:          node,
			CompositionReference: t.Composition,
		})
	}

	doc := manifestDoc{
		APIVersion: manifestAPIVersion,
		Kind:       sdkmanifest.ToolDefinitionsKind,
		Metadata: metadataBlock{
			Name:        meta.Name,
			DisplayName: meta.DisplayName,
			Description: meta.Description,
			Version:     meta.Version,
			Tier:        meta.Tier,
			Domain:      meta.Domain,
		},
		Spec: specBlock{
			Sidecar: sidecarBlock{
				Name:      meta.Sidecar.Name,
				Image:     meta.Sidecar.Image,
				Port:      meta.Sidecar.Port,
				Resources: resourceBlock{MemoryMB: meta.Sidecar.MemoryMB, CPUMilli: meta.Sidecar.CPUMilli},
			},
			Tools: blocks,
		},
	}

	var buf bytes.Buffer
	if h := meta.HeaderComment; h != "" {
		buf.WriteString(h)
		if h[len(h)-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// EmitToolsManifestFile renders the manifest and writes it to path. It is the
// one-call entry point for a kit's `go generate` step (see Runtime.Main).
func EmitToolsManifestFile(tools []Tool, meta ManifestMeta, path string) error {
	data, err := RenderToolsManifest(tools, meta)
	if err != nil {
		return err
	}
	// 0o600, not 0o644: the manifest is a generate-time artifact the kit author
	// commits (see the //go:generate step in the kit's tools cmd), and git records
	// no mode beyond the exec bit -- so every downstream reader gets its mode from
	// checkout, not from here. The write mode only ever applies to the first
	// creation on the authoring machine, where owner-only is sufficient. Please
	// don't widen it back: gosec/semgrep G302 flags anything above 0o640.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// manifestDescriptions returns the localized tool-level description map for the
// manifest: the explicit DescriptionI18n, else {"en-US": Description} when only
// the plain string is set.
func manifestDescriptions(t Tool) map[string]string {
	if len(t.DescriptionI18n) > 0 {
		return t.DescriptionI18n
	}
	if t.Description != "" {
		return map[string]string{"en-US": t.Description}
	}
	return nil
}

// schemaToNode parses a tool's JSON Schema into a YAML node, preserving JSON key
// order so the rendered YAML mirrors the authored schema. JSON is a subset of
// YAML, so yaml.Unmarshal decodes it directly (and types integers as ints).
func schemaToNode(raw json.RawMessage) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse input schema: %w", err)
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0], nil
	}
	return &doc, nil
}
