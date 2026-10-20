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

// platformTools mirrors the names in the platform's toolscope.PlatformTools
// (oga-platform internal/toolscope/platform_tools.go), which is pinned to the
// tool server's registry. Sorted.
var platformTools = []string{
	"kg_aggregate",
	"kg_compare_versions",
	"kg_create_entity",
	"kg_create_relationship",
	"kg_delete_entity",
	"kg_delete_relationship",
	"kg_describe_type",
	"kg_doc_content",
	"kg_doc_delete",
	"kg_doc_search",
	"kg_doc_status",
	"kg_doc_upload",
	"kg_entity_versions",
	"kg_geotemporal",
	"kg_get_entity",
	"kg_get_type_hierarchy",
	"kg_ops_alerts",
	"kg_ops_checklist",
	"kg_ops_dispatch",
	"kg_ops_escalate",
	"kg_ops_handover",
	"kg_ops_monitor",
	"kg_query_entities",
	"kg_query_relationships",
	"kg_reason",
	"kg_search",
	"kg_search_entity_types",
	"kg_search_relationship_types",
	"kg_set_external_ref",
	"kg_traverse",
	"kg_ts_analyze",
	"kg_ts_metrics",
	"kg_ts_read",
	"kg_update_entity",
	"kg_vector",
}

// PlatformTools returns the platform tools a kit may declare, sorted.
func PlatformTools() []string {
	return slices.Clone(platformTools)
}

// IsPlatformTool reports whether name is one of the platform's tools.
func IsPlatformTool(name string) bool {
	_, found := slices.BinarySearch(platformTools, name)
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
