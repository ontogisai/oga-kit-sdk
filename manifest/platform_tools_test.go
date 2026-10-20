package manifest

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The table is sorted (IsPlatformTool searches it), has no duplicates, and
// holds only kg_* names.
func TestPlatformTools_Table(t *testing.T) {
	tools := PlatformTools()
	if len(tools) == 0 {
		t.Fatal("empty platform tool table")
	}
	if !slices.IsSorted(tools) {
		t.Error("the table is not sorted")
	}
	for i, name := range tools {
		if !strings.HasPrefix(name, PlatformToolPrefix) {
			t.Errorf("%q is outside the kg_ namespace", name)
		}
		if i > 0 && tools[i-1] == name {
			t.Errorf("duplicate %q", name)
		}
		if !IsPlatformTool(name) {
			t.Errorf("IsPlatformTool(%q) = false", name)
		}
	}
	tools[0] = "changed"
	if PlatformTools()[0] == "changed" {
		t.Error("PlatformTools returns the table itself, not a copy")
	}
	for _, name := range []string{"", "kg_get_entities", "loader.complete", "fm_create_work_order"} {
		if IsPlatformTool(name) {
			t.Errorf("IsPlatformTool(%q) = true", name)
		}
	}
}

func TestValidateCompositionReference(t *testing.T) {
	if err := ValidateCompositionReference("kit-tools", "fm_tool", []string{"kg_get_entity", "kg_create_entity"}); err != nil {
		t.Fatalf("platform tools refused: %v", err)
	}
	if err := ValidateCompositionReference("kit-tools", "fm_tool", nil); err != nil {
		t.Fatalf("an empty composition refused: %v", err)
	}
	cases := []struct {
		name, tool, reason string
	}{
		{"loader operation", "loader.complete", "loader operations"},
		{"unknown kg_ tool", "kg_get_entities", "may name only platform tools"},
		{"kit tool", "fm_other", "may name only platform tools"},
		{"empty name", "", "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateCompositionReference("kit-tools", "fm_tool", []string{"kg_get_entity", c.tool})
			var ge *ToolGrantError
			if !errors.As(err, &ge) {
				t.Fatalf("err = %v, want *ToolGrantError", err)
			}
			if ge.Tool != c.tool || ge.Field != CompositionToolsField || !strings.Contains(ge.Reason, c.reason) {
				t.Errorf("err = %+v", ge)
			}
			want := `kit tool grant refused: tool "fm_tool" in tool file "kit-tools" names ` + c.tool + " in " + CompositionToolsField + ": "
			if !strings.HasPrefix(err.Error(), want) {
				t.Errorf("message %q, want the platform's form %q…", err.Error(), want)
			}
		})
	}
}

func TestValidateAgentTools(t *testing.T) {
	kitTools := []string{"fm_create_work_order", "kg_custom"}
	if err := ValidateAgentTools("fm-operations", []string{"kg_get_entity", "fm_create_work_order"}, kitTools); err != nil {
		t.Fatalf("a platform tool and a kit tool refused: %v", err)
	}
	cases := []struct {
		name, tool, reason string
	}{
		{"loader operation", "loader.status", "loader operations"},
		{"unknown name", "fm_typo", "its kit's own Tier-3 tools"},
		{"kit tool in the kg_ namespace", "kg_custom", "its kit's own Tier-3 tools"},
		{"empty name", "", "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateAgentTools("fm-operations", []string{c.tool}, kitTools)
			var ge *ToolGrantError
			if !errors.As(err, &ge) {
				t.Fatalf("err = %v, want *ToolGrantError", err)
			}
			if ge.Tool != c.tool || ge.Field != AgentToolsField || ge.Owner != `agent profile "fm-operations"` || !strings.Contains(ge.Reason, c.reason) {
				t.Errorf("err = %+v", ge)
			}
		})
	}
}

// ParseToolDefinitions applies the composition rules, as the platform does at
// upload and install.
func TestParseToolDefinitions_CompositionReference(t *testing.T) {
	doc := func(refs string) []byte {
		return []byte("api_version: ontogis.ai/v1\nkind: MCPToolDefinitions\nmetadata:\n  name: kit-tools\nspec:\n  tools:\n" +
			"    - name: fm_tool\n      description: {en-US: a tool}\n      input_schema: {type: object}\n" +
			"      composition_reference: [" + refs + "]\n")
	}
	if _, err := ParseToolDefinitions(doc("kg_get_entity, kg_traverse")); err != nil {
		t.Fatalf("valid composition refused: %v", err)
	}
	_, err := ParseToolDefinitions(doc("kg_get_entity, loader.complete"))
	var ge *ToolGrantError
	if !errors.As(err, &ge) || ge.Tool != "loader.complete" {
		t.Errorf("a loader operation in composition_reference: err = %v", err)
	}
}
