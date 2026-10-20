package manifest

import (
	"fmt"
	"slices"
	"strings"
)

// Declared tool grants (OGA-1121).
//
// The Platform Gateway lets a kit agent or MCP server sidecar call the platform
// tools its kit declares, and refuses any other tool:
//
//   - an agent: the union of its profile's spec.capabilities[].tools. A name is
//     a platform tool (kg_*) or one of the kit's own Tier-3 tools;
//   - an MCP server: the union of its tools' composition_reference lists
//     (mcptools.Tool.Composition). A name is a platform tool.
//
// The platform refuses a kit whose declarations break these rules at upload,
// install and upgrade (OGA-DKIT-VAL-1056). ValidateAgentTools and
// ValidateCompositionReference apply the same rules locally, with the same
// message, so a kit author finds the problem in `go test`.

// Platform tool namespaces.
const (
	// PlatformToolPrefix is the namespace of the platform's tools.
	PlatformToolPrefix = "kg_"
	// LoaderToolPrefix is the namespace of the loader operations. A loader or
	// source connector is granted those by its kind; no kit declares them.
	LoaderToolPrefix = "loader."
)

// Fields named in a ToolGrantError.
const (
	AgentToolsField       = "spec.capabilities[].tools"
	CompositionToolsField = "spec.tools[].composition_reference"
)

// PlatformTool is one tool the platform's MCP tool server registers.
type PlatformTool struct {
	Name string
	// Mutates is the tool server's flag: the tool changes the knowledge graph.
	Mutates bool
}

// platformTools mirrors the platform's toolscope.PlatformTools (oga-platform
// internal/toolscope/platform_tools.go), which is pinned to the tool server's
// registry. Sorted by name.
var platformTools = []PlatformTool{
	{Name: "kg_aggregate"},
	{Name: "kg_compare_versions"},
	{Name: "kg_create_entity", Mutates: true},
	{Name: "kg_create_relationship", Mutates: true},
	{Name: "kg_delete_entity", Mutates: true},
	{Name: "kg_delete_relationship", Mutates: true},
	{Name: "kg_describe_type"},
	{Name: "kg_doc_content"},
	{Name: "kg_doc_delete"},
	{Name: "kg_doc_search"},
	{Name: "kg_doc_status"},
	{Name: "kg_doc_upload"},
	{Name: "kg_entity_versions"},
	{Name: "kg_geotemporal"},
	{Name: "kg_get_entity"},
	{Name: "kg_get_type_hierarchy"},
	{Name: "kg_ops_checklist"},
	{Name: "kg_ops_dispatch"},
	{Name: "kg_ops_escalate"},
	{Name: "kg_ops_handover"},
	{Name: "kg_ops_monitor"},
	{Name: "kg_query_entities"},
	{Name: "kg_query_relationships"},
	{Name: "kg_reason"},
	{Name: "kg_search"},
	{Name: "kg_search_entity_types"},
	{Name: "kg_search_relationship_types"},
	{Name: "kg_set_external_ref", Mutates: true},
	{Name: "kg_traverse"},
	{Name: "kg_ts_analyze"},
	{Name: "kg_ts_metrics"},
	{Name: "kg_ts_read"},
	{Name: "kg_update_entity", Mutates: true},
	{Name: "kg_vector"},
}

// PlatformTools returns the platform tools a kit may declare, sorted by name.
func PlatformTools() []PlatformTool {
	return slices.Clone(platformTools)
}

// IsPlatformTool reports whether name is one of the platform's tools.
func IsPlatformTool(name string) bool {
	_, found := slices.BinarySearchFunc(platformTools, name, func(t PlatformTool, n string) int {
		return strings.Compare(t.Name, n)
	})
	return found
}

// ToolGrantError is a tool-grant declaration the platform refuses
// (OGA-DKIT-VAL-1056). Error() has the platform's message.
type ToolGrantError struct {
	// Owner names the declaring agent profile or tool.
	Owner string
	// Field is AgentToolsField or CompositionToolsField.
	Field string
	// Tool is the refused name.
	Tool string
	// Reason says why.
	Reason string
}

func (e *ToolGrantError) Error() string {
	return fmt.Sprintf("kit tool grant refused: %s names %s in %s: %s", e.Owner, e.Tool, e.Field, e.Reason)
}

// ValidateCompositionReference checks one tool's composition_reference: every
// name must be a platform tool. fileName is the tool file's metadata.name.
func ValidateCompositionReference(fileName, toolName string, refs []string) error {
	for _, name := range refs {
		if reason, ok := refuseDeclaredTool(name, nil); !ok {
			return &ToolGrantError{
				Owner:  fmt.Sprintf("tool %q in tool file %q", toolName, fileName),
				Field:  CompositionToolsField,
				Tool:   name,
				Reason: reason,
			}
		}
	}
	return nil
}

// ValidateAgentTools checks an agent profile's capabilities[].tools: every
// name must be a platform tool or one of kitTools, the names of the Tier-3
// tools the same kit defines.
func ValidateAgentTools(agentName string, tools, kitTools []string) error {
	kit := make(map[string]bool, len(kitTools))
	for _, t := range kitTools {
		if t != "" {
			kit[t] = true
		}
	}
	for _, name := range tools {
		if reason, ok := refuseDeclaredTool(name, kit); !ok {
			return &ToolGrantError{
				Owner:  fmt.Sprintf("agent profile %q", agentName),
				Field:  AgentToolsField,
				Tool:   name,
				Reason: reason,
			}
		}
	}
	return nil
}

// refuseDeclaredTool decides one declared name, as the platform does. kitTools
// is nil for a composition_reference and the kit's tool names for an agent.
func refuseDeclaredTool(name string, kitTools map[string]bool) (string, bool) {
	switch {
	case name == "":
		return "the tool name is empty", false
	case strings.HasPrefix(name, LoaderToolPrefix):
		return "the loader operations are granted by a sidecar's kind and are never declared", false
	case IsPlatformTool(name):
		return "", true
	case kitTools == nil:
		return "an MCP server's tools may name only platform tools (kg_*)", false
	case kitTools[name] && !strings.HasPrefix(name, PlatformToolPrefix):
		return "", true
	default:
		return "an agent may name only platform tools (kg_*) and its kit's own Tier-3 tools", false
	}
}
