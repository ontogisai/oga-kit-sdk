package streampipeline

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// fakeListerGateway is a PlatformAccess that also implements toolSchemaLister.
type fakeListerGateway struct {
	PlatformAccess
	calls   atomic.Int32
	tools   []gateway.ToolSchema
	listErr error
}

func (f *fakeListerGateway) ListTools(_ context.Context) ([]gateway.ToolSchema, error) {
	f.calls.Add(1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

// nonListerGateway is a PlatformAccess that does NOT implement toolSchemaLister.
type nonListerGateway struct{ PlatformAccess }

func tsTool(name string, required ...string) gateway.ToolSchema {
	schema := map[string]any{"type": "object"}
	if len(required) > 0 {
		ifaces := make([]any, len(required))
		for i, r := range required {
			ifaces[i] = r
		}
		schema["required"] = ifaces
	}
	raw, _ := json.Marshal(schema)
	return gateway.ToolSchema{Name: name, Description: name + " desc", InputSchema: raw}
}

// TestSchemasFor_ReturnsDiscoveredSchemasForPaletteTools verifies the cache
// returns schemas for the requested palette tools, carrying the input schema.
func TestSchemasFor_ReturnsDiscoveredSchemasForPaletteTools(t *testing.T) {
	gw := &fakeListerGateway{tools: []gateway.ToolSchema{
		tsTool("kg_ts_read", "mode", "from", "metric"),
		tsTool("kg_doc_content"),
		tsTool("kg_admin_secret"),
	}}
	cache := newToolSchemaCache()

	got := cache.schemasFor(context.Background(), gw, []string{"kg_ts_read", "kg_doc_content"})
	if len(got) != 2 {
		t.Fatalf("got %d schemas, want 2: %+v", len(got), got)
	}
	byName := map[string]agent.ToolSchema{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if _, ok := byName["kg_admin_secret"]; ok {
		t.Error("returned a tool that was not in the requested palette")
	}
	var req struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(byName["kg_ts_read"].InputSchema, &req); err != nil {
		t.Fatalf("kg_ts_read schema not valid JSON: %v", err)
	}
	if len(req.Required) != 3 {
		t.Errorf("kg_ts_read required = %v, want 3 fields", req.Required)
	}
}

// TestSchemasFor_CachesAcrossCalls verifies discovery is memoized within TTL.
func TestSchemasFor_CachesAcrossCalls(t *testing.T) {
	gw := &fakeListerGateway{tools: []gateway.ToolSchema{tsTool("kg_ts_read", "mode")}}
	cache := newToolSchemaCache()

	for i := 0; i < 3; i++ {
		_ = cache.schemasFor(context.Background(), gw, []string{"kg_ts_read"})
	}
	if n := gw.calls.Load(); n != 1 {
		t.Fatalf("ListTools called %d times, want 1 (cached)", n)
	}
}

// TestSchemasFor_GatewayWithoutDiscovery degrades to nil (names-only).
func TestSchemasFor_GatewayWithoutDiscovery(t *testing.T) {
	cache := newToolSchemaCache()
	got := cache.schemasFor(context.Background(), &nonListerGateway{}, []string{"kg_ts_read"})
	if got != nil {
		t.Fatalf("expected nil for a gateway without discovery, got %+v", got)
	}
}

// TestSchemasFor_DiscoveryErrorDegradesGracefully returns nil on a list error.
func TestSchemasFor_DiscoveryErrorDegradesGracefully(t *testing.T) {
	gw := &fakeListerGateway{listErr: errors.New("backend down")}
	cache := newToolSchemaCache()
	got := cache.schemasFor(context.Background(), gw, []string{"kg_ts_read"})
	if got != nil {
		t.Fatalf("expected nil on discovery error, got %+v", got)
	}
}

// TestMergeToolSchemas_DiscoveredWins verifies delegation schemas are appended
// and discovered entries win on name collision.
func TestMergeToolSchemas_DiscoveredWins(t *testing.T) {
	discovered := []agent.ToolSchema{{Name: "kg_ts_read", Description: "discovered"}}
	extra := []agent.ToolSchema{
		{Name: "kg_ts_read", Description: "dup-should-lose"},
		{Name: "ask_knowledge_agent", Description: "delegation"},
	}
	merged := mergeToolSchemas(discovered, extra)
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2", len(merged))
	}
	for _, s := range merged {
		if s.Name == "kg_ts_read" && s.Description != "discovered" {
			t.Errorf("discovered entry should win, got %q", s.Description)
		}
	}
}

// TestSchemasFor_CarriesMutatesFlag verifies the OGA-446 mutation flag flows
// from the gateway tools/list discovery into the planner's ToolSchema, so kit
// agents get the platform's authoritative confirm-before-write signal instead
// of relying on the name heuristic.
func TestSchemasFor_CarriesMutatesFlag(t *testing.T) {
	tt := true
	writer := gateway.ToolSchema{Name: "fm_create_work_order", Description: "d", Mutates: &tt}
	reader := gateway.ToolSchema{Name: "kg_search", Description: "d"} // Mutates nil → heuristic
	gw := &fakeListerGateway{tools: []gateway.ToolSchema{writer, reader}}
	cache := newToolSchemaCache()

	got := cache.schemasFor(context.Background(), gw, []string{"fm_create_work_order", "kg_search"})
	byName := map[string]agent.ToolSchema{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if m := byName["fm_create_work_order"].Mutates; m == nil || !*m {
		t.Errorf("fm_create_work_order Mutates = %v, want explicit true", m)
	}
	if byName["kg_search"].Mutates != nil {
		t.Errorf("kg_search Mutates = %v, want nil (absent → heuristic)", byName["kg_search"].Mutates)
	}
	// End-to-end: toolMutates honours the explicit flag.
	schemas := map[string]agent.ToolSchema{"fm_create_work_order": byName["fm_create_work_order"]}
	if !toolMutates(schemas, "fm_create_work_order") {
		t.Error("toolMutates should report true for an explicitly-flagged writer")
	}
}
