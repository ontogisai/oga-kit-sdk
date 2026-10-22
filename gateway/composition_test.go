package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// countingGateway answers every tool call with an empty result and counts the
// requests it received.
func countingGateway(t *testing.T) (*PlatformGatewayClient, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return NewPlatformGatewayClient(srv.URL, "t1"), &calls
}

// Inside a tool handler, a platform tool outside the running tool's
// composition is refused locally and nothing is sent; a tool in it is sent.
func TestCallTool_ComposedToolsOnly(t *testing.T) {
	gw, calls := countingGateway(t)
	ctx := WithToolComposition(context.Background(), "fm_overview", []string{"kg_get_entity"})

	if _, err := gw.CallTool(ctx, "kg_get_entity", map[string]any{}); err != nil {
		t.Fatalf("composed tool: %v", err)
	}
	_, err := gw.CallTool(ctx, "kg_create_entity", map[string]any{})
	var ce *CompositionError
	if !errors.As(err, &ce) || ce.Tool != "kg_create_entity" || ce.RunningTool != "fm_overview" {
		t.Fatalf("err = %v, want *CompositionError for kg_create_entity in fm_overview", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("gateway received %d requests, want 1 (the refused call must not be sent)", n)
	}
}

// A tool with no composition may call no platform tool.
func TestCallTool_EmptyCompositionAllowsNothing(t *testing.T) {
	gw, calls := countingGateway(t)
	ctx := WithToolComposition(context.Background(), "fm_pure", nil)
	if _, err := gw.CallTool(ctx, "kg_get_entity", nil); !errors.As(err, new(*CompositionError)) {
		t.Fatalf("err = %v, want *CompositionError", err)
	}
	if calls.Load() != 0 {
		t.Error("a refused call was sent")
	}
}

// Outside a tool handler (an agent, a loader) CallTool checks nothing.
func TestCallTool_NoCompositionOnContext(t *testing.T) {
	gw, calls := countingGateway(t)
	if _, err := gw.CallTool(context.Background(), "kg_create_entity", nil); err != nil {
		t.Fatalf("call without a composition: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("gateway received %d requests, want 1", calls.Load())
	}
	if _, _, ok := ToolCompositionFromContext(context.Background()); ok {
		t.Error("a bare context reports a composition")
	}
}

// The context keeps its own copy of the composition.
func TestWithToolComposition_Copies(t *testing.T) {
	list := []string{"kg_get_entity"}
	ctx := WithToolComposition(context.Background(), "fm_overview", list)
	list[0] = "kg_delete_entity"
	tool, got, ok := ToolCompositionFromContext(ctx)
	if !ok || tool != "fm_overview" || len(got) != 1 || got[0] != "kg_get_entity" {
		t.Errorf("composition = %q %v %v", tool, got, ok)
	}
}
