package gateway

import (
	"context"
	"fmt"
	"slices"
)

// Tool composition (OGA-1121).
//
// A kit MCP server tool declares, in its Composition (manifest
// composition_reference), the platform tools its handler calls. The Platform
// Gateway grants the sidecar the union over all its tools and refuses anything
// else; that is the security boundary. The SDK adds a local check per RUNNING
// tool: mcptools stamps the tool's composition on the handler's context, and
// CallTool refuses a platform tool the composition does not list, without a
// network call. It is developer feedback, so a handler that drifts from its
// declaration fails in the kit's own tests, and the gateway's answer for the
// sidecar never depends on which of its tools was running.

type compositionKey struct{}

type toolComposition struct {
	tool  string
	tools []string
}

// WithToolComposition returns ctx marked as the context of the kit tool named
// tool, whose handler may call only the platform tools in composition. A nil
// or empty composition allows no platform tool. mcptools sets it for every
// tools/call; a caller outside a tool handler leaves it unset and CallTool
// checks nothing.
func WithToolComposition(ctx context.Context, tool string, composition []string) context.Context {
	return context.WithValue(ctx, compositionKey{}, toolComposition{tool: tool, tools: slices.Clone(composition)})
}

// ToolCompositionFromContext returns the running tool's name and composition,
// and whether ctx carries one.
func ToolCompositionFromContext(ctx context.Context) (tool string, composition []string, ok bool) {
	c, ok := ctx.Value(compositionKey{}).(toolComposition)
	if !ok {
		return "", nil, false
	}
	return c.tool, slices.Clone(c.tools), true
}

// CompositionError is CallTool's local refusal of a platform tool the running
// kit tool does not list in its Composition. No request was sent.
type CompositionError struct {
	// RunningTool is the kit tool whose handler made the call.
	RunningTool string
	// Tool is the platform tool it tried to call.
	Tool string
}

func (e *CompositionError) Error() string {
	return fmt.Sprintf("tool %s is not in the Composition (composition_reference) of %s; "+
		"the sidecar's grant at the Platform Gateway is its tools' compositions", e.Tool, e.RunningTool)
}

// checkComposition refuses tool when ctx carries a running tool's composition
// that does not list it.
func checkComposition(ctx context.Context, tool string) error {
	c, ok := ctx.Value(compositionKey{}).(toolComposition)
	if !ok || slices.Contains(c.tools, tool) {
		return nil
	}
	return &CompositionError{RunningTool: c.tool, Tool: tool}
}
