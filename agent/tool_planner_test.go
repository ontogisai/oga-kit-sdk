package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// fakeGateway is a deterministic stand-in for *gateway.PlatformGatewayClient.
type fakeGateway struct {
	chatResponses []chatResponse
	chatCalls     []*gateway.ChatCompletionRequest
	chatIdx       int

	toolResponses map[string]toolResponse
	toolCalls     []toolCall
}

type chatResponse struct {
	content string
	err     error
}

type toolResponse struct {
	raw json.RawMessage
	err error
}

type toolCall struct {
	tool   string
	params any
}

func (f *fakeGateway) ChatCompletion(_ context.Context, req *gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	f.chatCalls = append(f.chatCalls, req)
	if f.chatIdx >= len(f.chatResponses) {
		return nil, errors.New("fakeGateway: no more chat responses")
	}
	r := f.chatResponses[f.chatIdx]
	f.chatIdx++
	if r.err != nil {
		return nil, r.err
	}
	return &gateway.ChatCompletionResponse{
		Choices: []gateway.ChatChoice{
			{Message: gateway.ChatMessage{Role: "assistant", Content: r.content}},
		},
	}, nil
}

func (f *fakeGateway) CallTool(_ context.Context, tool string, params any) (json.RawMessage, error) {
	f.toolCalls = append(f.toolCalls, toolCall{tool: tool, params: params})
	r, ok := f.toolResponses[tool]
	if !ok {
		return nil, errors.New("fakeGateway: no response for tool " + tool)
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.raw, nil
}

func sampleProfile() *DomainAgentProfile {
	return &DomainAgentProfile{
		AgentID:     "fm-operations-agent",
		Name:        "fm-operations-agent",
		Description: "FM ops agent",
		Capabilities: []CapabilityDef{
			{Name: "work_order_management", Tools: []string{"kg_entity_search", "kg_traversal"}},
			{Name: "equipment_status", Tools: []string{"kg_timeseries_query", "kg_entity_search"}},
		},
		ProactiveReasoning: &ProactiveConfig{
			SystemPrompt: "You manage facility equipment.",
		},
	}
}

func TestUniqueTools_Dedup(t *testing.T) {
	profile := sampleProfile()
	tools := UniqueTools(profile)
	if len(tools) != 3 {
		t.Fatalf("expected 3 unique tools, got %d (%v)", len(tools), tools)
	}
	// Order from first occurrence in capabilities
	want := []string{"kg_entity_search", "kg_traversal", "kg_timeseries_query"}
	for i, w := range want {
		if tools[i] != w {
			t.Errorf("tools[%d] = %q, want %q", i, tools[i], w)
		}
	}
}

func TestUniqueTools_NilOrEmpty(t *testing.T) {
	if got := UniqueTools(nil); got != nil {
		t.Errorf("nil profile should yield nil, got %v", got)
	}
	empty := &DomainAgentProfile{}
	if got := UniqueTools(empty); got != nil {
		t.Errorf("profile without capabilities should yield nil, got %v", got)
	}
}

func TestParsePlan_ValidJSON(t *testing.T) {
	content := `{"steps":[{"tool_name":"kg_search","arguments":{"q":"chiller"},"depends_on":-1,"rationale":"find equipment"}]}`
	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(plan.Steps))
	}
	if plan.Steps[0].ToolName != "kg_search" {
		t.Errorf("tool_name = %q, want kg_search", plan.Steps[0].ToolName)
	}
}

func TestParsePlan_StripsMarkdownFences(t *testing.T) {
	content := "```json\n{\"steps\":[]}\n```"
	plan, err := ParsePlan(content)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("expected empty steps, got %d", len(plan.Steps))
	}
}

func TestParsePlan_InvalidJSON(t *testing.T) {
	if _, err := ParsePlan("not json"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestPlanAndExecute_FullLoop_HappyPath(t *testing.T) {
	planJSON := `{"steps":[{"tool_name":"kg_entity_search","arguments":{"q":"chiller CH-01"},"depends_on":-1,"rationale":"find equipment"}]}`
	gw := &fakeGateway{
		chatResponses: []chatResponse{
			{content: planJSON},                             // planner
			{content: "Chiller CH-01 was last serviced..."}, // assembler
		},
		toolResponses: map[string]toolResponse{
			"kg_entity_search": {raw: json.RawMessage(`{"entities":[{"id":"ch-01","name":"Chiller CH-01"}]}`)},
		},
	}

	answer, results, err := PlanAndExecute(context.Background(), gw, sampleProfile(), "tell me about chiller CH-01", DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("PlanAndExecute: %v", err)
	}
	if !strings.Contains(answer, "CH-01") {
		t.Errorf("answer should mention CH-01, got: %q", answer)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("expected success, got error: %s", results[0].Error)
	}
	if len(gw.chatCalls) != 2 {
		t.Errorf("expected 2 chat calls (plan + assemble), got %d", len(gw.chatCalls))
	}
	if len(gw.toolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(gw.toolCalls))
	}
	if gw.toolCalls[0].tool != "kg_entity_search" {
		t.Errorf("tool = %q, want kg_entity_search", gw.toolCalls[0].tool)
	}
}

func TestPlanAndExecute_EmptyPlan_FallsBackToPlain(t *testing.T) {
	// Planner returns no steps -> fall back to plain answer.
	gw := &fakeGateway{
		chatResponses: []chatResponse{
			{content: `{"steps":[]}`}, // planner returns empty plan
			{content: "I can help with FM ops."},
		},
	}

	answer, results, err := PlanAndExecute(context.Background(), gw, sampleProfile(), "hello", DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("PlanAndExecute: %v", err)
	}
	if answer == "" {
		t.Error("expected non-empty answer")
	}
	if len(results) != 0 {
		t.Errorf("expected no results, got %d", len(results))
	}
	if len(gw.chatCalls) != 2 {
		t.Errorf("expected 2 chat calls (plan + plain), got %d", len(gw.chatCalls))
	}
	if len(gw.toolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(gw.toolCalls))
	}
}

func TestPlanAndExecute_PlannerFails_FallsBackToPlain(t *testing.T) {
	gw := &fakeGateway{
		chatResponses: []chatResponse{
			{err: errors.New("planner LLM unavailable")},
			{content: "I can help, but I cannot access the knowledge graph right now."},
		},
	}

	answer, results, err := PlanAndExecute(context.Background(), gw, sampleProfile(), "what equipment needs maintenance?", DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("PlanAndExecute should fall back, got error: %v", err)
	}
	if answer == "" {
		t.Error("expected fallback answer")
	}
	if len(results) != 0 {
		t.Errorf("expected no results from fallback, got %d", len(results))
	}
}

func TestPlanAndExecute_NoToolsInProfile_FallsBackToPlain(t *testing.T) {
	profile := &DomainAgentProfile{
		Name: "no-tools-agent",
		// No capabilities -> no tools
	}
	gw := &fakeGateway{
		chatResponses: []chatResponse{
			{content: "I can chat but cannot query data."},
		},
	}

	answer, results, err := PlanAndExecute(context.Background(), gw, profile, "hello", DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("PlanAndExecute: %v", err)
	}
	if answer == "" {
		t.Error("expected non-empty answer")
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
	// Only one call (no planning, no assembly — direct plain answer).
	if len(gw.chatCalls) != 1 {
		t.Errorf("expected 1 chat call, got %d", len(gw.chatCalls))
	}
}

func TestPlanAndExecute_ToolErrorStillAssembles(t *testing.T) {
	planJSON := `{"steps":[{"tool_name":"kg_entity_search","arguments":{"q":"x"},"depends_on":-1}]}`
	gw := &fakeGateway{
		chatResponses: []chatResponse{
			{content: planJSON},
			{content: "I tried to find that equipment but the search failed."},
		},
		toolResponses: map[string]toolResponse{
			"kg_entity_search": {err: errors.New("search backend timeout")},
		},
	}

	answer, results, err := PlanAndExecute(context.Background(), gw, sampleProfile(), "find x", DefaultPlannerConfig())
	if err != nil {
		t.Fatalf("PlanAndExecute should still assemble on tool error: %v", err)
	}
	if answer == "" {
		t.Error("expected assembled fallback answer")
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Success {
		t.Error("expected step result to be marked unsuccessful")
	}
	if results[0].Error != "search backend timeout" {
		t.Errorf("error = %q", results[0].Error)
	}
}

func TestPlanAndExecute_PlanExceedsMaxSteps_Truncates(t *testing.T) {
	// Plan with 8 steps; cfg.MaxSteps = 3 -> truncate to 3.
	planJSON := `{"steps":[
		{"tool_name":"kg_entity_search","arguments":{"q":"a"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"b"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"c"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"d"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"e"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"f"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"g"},"depends_on":-1},
		{"tool_name":"kg_entity_search","arguments":{"q":"h"},"depends_on":-1}
	]}`
	gw := &fakeGateway{
		chatResponses: []chatResponse{
			{content: planJSON},
			{content: "ok"},
		},
		toolResponses: map[string]toolResponse{
			"kg_entity_search": {raw: json.RawMessage(`{"ok":true}`)},
		},
	}

	cfg := DefaultPlannerConfig()
	cfg.MaxSteps = 3

	_, results, err := PlanAndExecute(context.Background(), gw, sampleProfile(), "many things", cfg)
	if err != nil {
		t.Fatalf("PlanAndExecute: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results after truncation, got %d", len(results))
	}
	if len(gw.toolCalls) != 3 {
		t.Errorf("expected 3 tool calls, got %d", len(gw.toolCalls))
	}
}
