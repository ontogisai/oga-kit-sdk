package manifest

import (
	"strings"
	"testing"
)

const validToolDefs = `
api_version: ontogis.ai/v1
kind: MCPToolDefinitions
metadata:
  name: example-tier3-tools
spec:
  sidecar:
    image: ghcr.io/ontogisai/example/tools-mcp:0.0.0-dev
    port: 8300
  tools:
    - name: fm_get_building_overview
      display_name:
        en-US: "Get Building Overview"
      description:
        en-US: "Returns a deterministic overview of a building."
      input_schema:
        type: object
        required: [building_id]
        properties:
          building_id:
            type: string
            description: "Entity ID of the IfcBuilding"
          measurement_types:
            type: array
            items:
              type: string
              description: "A measurement type"
`

func TestParseToolDefinitions_Valid(t *testing.T) {
	tools, err := ParseToolDefinitions([]byte(validToolDefs))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "fm_get_building_overview" {
		t.Fatalf("unexpected parse result: %+v", tools)
	}
}

// TestParseToolDefinitions_RejectsObjectPropertyDescription is the OGA-582
// regression: a property-level description declared as a localized object
// (`{ en-US: "..." }`) must be rejected at author time, not silently dropped at
// catalog registration.
func TestParseToolDefinitions_RejectsObjectPropertyDescription(t *testing.T) {
	doc := `
api_version: ontogis.ai/v1
kind: MCPToolDefinitions
metadata:
  name: example-tier3-tools
spec:
  tools:
    - name: fm_get_building_overview
      description:
        en-US: "Returns a building overview."
      input_schema:
        type: object
        properties:
          building_id:
            type: string
            description:
              en-US: "Entity ID of the IfcBuilding"
`
	_, err := ParseToolDefinitions([]byte(doc))
	if err == nil {
		t.Fatal("expected error for object-valued property description")
	}
	if !strings.Contains(err.Error(), "building_id") || !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("error should name the offending property and reason, got: %v", err)
	}
}

// TestParseToolDefinitions_RejectsObjectDescriptionInArrayItems confirms the
// recursion descends into array items schemas.
func TestParseToolDefinitions_RejectsObjectDescriptionInArrayItems(t *testing.T) {
	doc := `
api_version: ontogis.ai/v1
kind: MCPToolDefinitions
metadata:
  name: example-tier3-tools
spec:
  tools:
    - name: fm_get_zone_sensors
      description:
        en-US: "Returns zone sensor readings."
      input_schema:
        type: object
        properties:
          measurement_types:
            type: array
            items:
              type: string
              description:
                en-US: "A measurement type"
`
	_, err := ParseToolDefinitions([]byte(doc))
	if err == nil {
		t.Fatal("expected error for object-valued description inside array items")
	}
	if !strings.Contains(err.Error(), "items") {
		t.Errorf("error should reference the items path, got: %v", err)
	}
}

// TestParseToolDefinitions_AllowsLocalizedToolLevelDescription confirms the
// tool-LEVEL description (distinct from a JSON Schema property description) may
// still be a localized map.
func TestParseToolDefinitions_AllowsLocalizedToolLevelDescription(t *testing.T) {
	doc := `
api_version: ontogis.ai/v1
kind: MCPToolDefinitions
metadata:
  name: example-tier3-tools
spec:
  tools:
    - name: fm_get_equipment_status
      description:
        en-US: "Equipment status."
        vi-VN: "Trạng thái thiết bị."
      input_schema:
        type: object
        properties:
          equipment_id:
            type: string
            description: "Entity ID of the equipment"
`
	if _, err := ParseToolDefinitions([]byte(doc)); err != nil {
		t.Fatalf("localized tool-level description should be valid, got: %v", err)
	}
}

func TestParseToolDefinitions_RejectsWrongKind(t *testing.T) {
	doc := `
api_version: ontogis.ai/v1
kind: NotToolDefinitions
spec:
  tools: []
`
	if _, err := ParseToolDefinitions([]byte(doc)); err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestParseToolDefinitions_EmptyToolListValid(t *testing.T) {
	doc := `
api_version: ontogis.ai/v1
kind: MCPToolDefinitions
metadata:
  name: example-tier3-tools
spec:
  tools: []
`
	tools, err := ParseToolDefinitions([]byte(doc))
	if err != nil {
		t.Fatalf("empty tool list should be valid, got: %v", err)
	}
	if tools != nil {
		t.Errorf("expected nil tools for empty list, got %v", tools)
	}
}
