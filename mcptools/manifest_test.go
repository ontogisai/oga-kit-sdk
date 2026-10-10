package mcptools_test

import (
	"encoding/json"
	"strings"
	"testing"

	sdkmanifest "github.com/ontogisai/oga-kit-sdk/manifest"
	"github.com/ontogisai/oga-kit-sdk/mcptools"
)

func sampleMeta() mcptools.ManifestMeta {
	return mcptools.ManifestMeta{
		Name:        "sample-tier3-tools",
		DisplayName: map[string]string{"en-US": "Sample Tools"},
		Description: map[string]string{"en-US": "Sample Tier 3 tools"},
		Version:     "0.0.0-dev",
		Tier:        3,
		Domain:      "sample",
		Sidecar: mcptools.SidecarMeta{
			Name:     "sample-tools-mcp",
			Image:    "ghcr.io/ontogisai/sample/sample-tools-mcp:0.0.0-dev",
			Port:     8300,
			MemoryMB: 128,
			CPUMilli: 100,
		},
		HeaderComment: "# Code generated; DO NOT EDIT.",
	}
}

func goodTools() []mcptools.Tool {
	return []mcptools.Tool{
		{
			Name:            "do_thing",
			DisplayNameI18n: map[string]string{"en-US": "Do Thing", "vi-VN": "Làm Việc"},
			DescriptionI18n: map[string]string{"en-US": "Does a thing deterministically"},
			Mutates:         true,
			Composition:     []string{"kg_get_entity"},
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["id"],
				"properties": {
					"id": {"type": "string", "description": "Entity id"},
					"opts": {"type": "array", "items": {"type": "string", "description": "an option"}}
				}
			}`),
		},
	}
}

// TestRenderToolsManifest_RoundTripsThroughValidator renders a manifest and
// confirms it passes the SDK validator (the same check the platform runs at
// install/upload) — the core OGA-582 guarantee: what a kit declares in Go emits
// a manifest the platform accepts.
func TestRenderToolsManifest_RoundTripsThroughValidator(t *testing.T) {
	data, err := mcptools.RenderToolsManifest(goodTools(), sampleMeta())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	tools, err := sdkmanifest.ParseToolDefinitions(data)
	if err != nil {
		t.Fatalf("rendered manifest failed SDK validation: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "do_thing" {
		t.Fatalf("parsed tools = %+v, want [do_thing]", tools)
	}
	if !strings.Contains(string(data), "# Code generated; DO NOT EDIT.") {
		t.Error("header comment not emitted")
	}
}

// TestRenderToolsManifest_RejectsObjectPropertyDescription is the OGA-582
// regression at the emitter: a localized-object property description fails at
// render/`go generate` time, never reaching a committed manifest.
func TestRenderToolsManifest_RejectsObjectPropertyDescription(t *testing.T) {
	tools := []mcptools.Tool{
		{
			Name:            "bad",
			DescriptionI18n: map[string]string{"en-US": "d"},
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": {"en-US": "Entity id"}}
				}
			}`),
		},
	}
	_, err := mcptools.RenderToolsManifest(tools, sampleMeta())
	if err == nil {
		t.Fatal("expected render to reject object-valued property description")
	}
	if !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRenderToolsManifest_Deterministic confirms two renders are byte-identical
// (so the committed artifact can be drift-checked).
func TestRenderToolsManifest_Deterministic(t *testing.T) {
	a, err := mcptools.RenderToolsManifest(goodTools(), sampleMeta())
	if err != nil {
		t.Fatalf("render a: %v", err)
	}
	b, err := mcptools.RenderToolsManifest(goodTools(), sampleMeta())
	if err != nil {
		t.Fatalf("render b: %v", err)
	}
	if string(a) != string(b) {
		t.Error("render is not deterministic")
	}
}

// TestRenderToolsManifest_DuplicateName rejects a duplicate tool name.
func TestRenderToolsManifest_DuplicateName(t *testing.T) {
	dup := append(goodTools(), goodTools()...)
	if _, err := mcptools.RenderToolsManifest(dup, sampleMeta()); err == nil {
		t.Fatal("expected duplicate tool name to be rejected")
	}
}
