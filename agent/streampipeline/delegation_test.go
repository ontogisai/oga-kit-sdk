package streampipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

// subEvent marshals a sub-agent StreamEvent as the SSE `data:` JSON payload
// InvokeAgentStream yields.
func subEvent(t *testing.T, evtType agent.EventType, payload any) string {
	t.Helper()
	raw, err := json.Marshal(&agent.StreamEvent{
		TaskID:   "sub-task",
		Sequence: 1,
		SpanID:   "sub-span",
		Depth:    0,
		Actor:    agent.EventActor{Type: "sub_agent", ID: "knowledge-agent", DisplayName: "OGA Knowledge Agent"},
		Type:     evtType,
		Payload:  payload,
	})
	if err != nil {
		t.Fatalf("marshal sub event: %v", err)
	}
	return string(raw)
}

// TestPipeline_Delegation_StreamsAndObserves verifies the reactive delegation
// path (OGA-419 G3): when the planner selects a delegation tool, the loop emits
// task/agent_call, streams + re-parents the sub-agent's events, emits
// task/agent_result, and feeds the sub-agent's answer back as the turn's
// observation.
func TestPipeline_Delegation_StreamsAndObserves(t *testing.T) {
	gw := &fakeGateway{
		streamChunks: []string{"Final answer."},
		delegateRaw: []string{
			subEvent(t, agent.EventTypeReasoning, map[string]any{"text": "KA is searching..."}),
			subEvent(t, agent.EventTypeArtifact, map[string]any{"parts": []map[string]any{{"text": "Tower 2 has 7 floors."}}}),
			subEvent(t, agent.EventTypeCitation, map[string]any{"sources": []map[string]any{{"type": "entity", "id": "ent-7a3f", "label": "Tower 2"}}}),
			subEvent(t, agent.EventTypeStatus, map[string]any{"state": "completed"}),
		},
	}

	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "ask", ToolName: "ask_knowledge_agent", Arguments: map[string]any{"question": "How many floors in Tower 2?"}},
		},
	}

	events := runPipelineForTest(t, gw, planner, Input{
		Query: "Tell me about Tower 2",
		Actor: agent.EventActor{Type: "domain_agent", ID: "fm-operations-agent", DisplayName: "FM Ops"},
		Delegations: []AgentDelegation{
			{ToolName: "ask_knowledge_agent", AgentName: "knowledge-agent", Description: "Ask the KA"},
		},
	})

	if len(gw.delegateCalls) != 1 || gw.delegateCalls[0] != "knowledge-agent" {
		t.Fatalf("expected one delegation to knowledge-agent, got %v", gw.delegateCalls)
	}

	var sawCall, sawResult, sawSubReasoning, sawSubArtifact bool
	for _, e := range events {
		switch e.Type {
		case agent.EventTypeAgentCall:
			sawCall = true
		case agent.EventTypeAgentResult:
			sawResult = true
		case agent.EventTypeReasoning:
			// Forwarded sub-agent events carry map[string]any payloads (decoded
			// from the JSON wire), so inspect via marshaled JSON.
			if jsonContains(t, e.Payload, "KA is searching") {
				sawSubReasoning = true
				if e.ParentSpanID == "" {
					t.Error("forwarded sub-agent reasoning must be re-parented (parent_span_id set)")
				}
				if e.Actor.ID != "knowledge-agent" {
					t.Errorf("forwarded event should keep sub-agent actor, got %q", e.Actor.ID)
				}
			}
		case agent.EventTypeArtifact:
			if jsonContains(t, e.Payload, "Tower 2 has 7 floors") {
				sawSubArtifact = true
			}
		}
	}
	if !sawCall {
		t.Error("expected a task/agent_call event")
	}
	if !sawResult {
		t.Error("expected a task/agent_result event")
	}
	if !sawSubReasoning {
		t.Error("expected the sub-agent's reasoning to be forwarded")
	}
	if !sawSubArtifact {
		t.Error("expected the sub-agent's artifact to be forwarded")
	}

	for _, e := range events {
		if e.Type == agent.EventTypeAgentResult {
			p, ok := e.Payload.(*agent.AgentResultPayload)
			if !ok {
				t.Fatalf("agent_result payload type = %T", e.Payload)
			}
			if !p.Success {
				t.Errorf("expected successful agent_result, got error %q", p.Error)
			}
			if !strings.Contains(p.Summary, "Tower 2 has 7 floors") {
				t.Errorf("agent_result summary should echo the sub-agent answer, got %q", p.Summary)
			}
		}
	}
}

// TestPipeline_Delegation_InvokeError surfaces a delegation transport failure
// honestly: agent_result reports failure and the loop continues (no panic).
func TestPipeline_Delegation_InvokeError(t *testing.T) {
	gw := &fakeGateway{
		streamChunks: []string{"degraded answer"},
		delegateErr:  context.DeadlineExceeded,
	}
	planner := &scriptedPlanner{
		steps: []ToolPlanStep{
			{Name: "ask", ToolName: "ask_knowledge_agent", Arguments: map[string]any{"question": "x"}},
		},
	}

	events := runPipelineForTest(t, gw, planner, Input{
		Query: "q",
		Actor: agent.EventActor{Type: "domain_agent", ID: "fm", DisplayName: "FM"},
		Delegations: []AgentDelegation{
			{ToolName: "ask_knowledge_agent", AgentName: "knowledge-agent", Description: "Ask the KA"},
		},
	})

	var failed bool
	for _, e := range events {
		if e.Type == agent.EventTypeAgentResult {
			if p, ok := e.Payload.(*agent.AgentResultPayload); ok && !p.Success {
				failed = true
			}
		}
	}
	if !failed {
		t.Error("expected a failed task/agent_result on delegation transport error")
	}
}

// TestDecodeDelegatedEvent covers both the bare StreamEvent and the A2A
// JSON-RPC envelope shapes.
func TestDecodeDelegatedEvent(t *testing.T) {
	bare := subEvent(t, agent.EventTypeArtifact, map[string]any{"parts": []map[string]any{{"text": "hi"}}})
	if got := decodeDelegatedEvent(json.RawMessage(bare)); got == nil || got.Type != agent.EventTypeArtifact {
		t.Fatalf("bare decode failed: %+v", got)
	}

	enveloped := `{"jsonrpc":"2.0","id":1,"result":` + bare + `}`
	if got := decodeDelegatedEvent(json.RawMessage(enveloped)); got == nil || got.Type != agent.EventTypeArtifact {
		t.Fatalf("enveloped decode failed: %+v", got)
	}

	if got := decodeDelegatedEvent(json.RawMessage(`{"not":"an event"}`)); got != nil {
		t.Errorf("expected nil for non-event JSON, got %+v", got)
	}
}

// TestReactivePersona_DelegationPalette asserts the reactive persona includes
// delegation tools (so the planner can pick them) while proactive purity is
// preserved by construction (the proactive path builds its own persona without
// delegations).
func TestReactivePersona_DelegationPalette(t *testing.T) {
	profile := &agent.DomainAgentProfile{
		AgentID: "fm",
		Capabilities: []agent.CapabilityDef{
			{Tools: []string{"kg_search", "kg_traverse"}},
		},
	}

	// No delegations → palette is the profile tools only.
	plain := reactivePersona(context.Background(), newToolSchemaCache(), nil, profile, nil)
	if containsStr(plain.Tools, "ask_knowledge_agent") {
		t.Error("plain reactive persona must not contain a delegation tool")
	}

	// With delegation → palette includes it + a schema entry carrying its description.
	withDel := reactivePersona(context.Background(), newToolSchemaCache(), nil, profile, []AgentDelegation{
		{ToolName: "ask_knowledge_agent", AgentName: "knowledge-agent", Description: "Ask the KA"},
	})
	if !containsStr(withDel.Tools, "ask_knowledge_agent") {
		t.Error("reactive persona with delegation must include ask_knowledge_agent in the palette")
	}
	var hasSchema bool
	for _, s := range withDel.ToolSchemas {
		if s.Name == "ask_knowledge_agent" && s.Description != "" {
			hasSchema = true
		}
	}
	if !hasSchema {
		t.Error("delegation should contribute a described ToolSchema entry")
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// jsonContains marshals a payload (typed or map[string]any) and reports whether
// the result contains substr.
func jsonContains(t *testing.T, payload any, substr string) bool {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return strings.Contains(string(b), substr)
}

// TestEffectiveDelegations_ProfileConfig verifies the default-opt-out,
// config-driven Knowledge Agent delegation wiring (OGA-419): the KA delegation
// is added only when spec.reactive_delegation.knowledge_agent is true, is
// absent otherwise, and is deduplicated against an explicit handler-option
// delegation for the same tool.
func TestEffectiveDelegations_ProfileConfig(t *testing.T) {
	t.Parallel()

	hasKA := func(ds []AgentDelegation) bool {
		for _, d := range ds {
			if d.ToolName == "ask_knowledge_agent" {
				return true
			}
		}
		return false
	}

	// Default opt-out: nil profile / nil config / false → no KA delegation.
	if got := effectiveDelegations(nil, nil); hasKA(got) {
		t.Error("nil profile must not enable KA delegation")
	}
	if got := effectiveDelegations(&agent.DomainAgentProfile{}, nil); hasKA(got) {
		t.Error("profile without reactive_delegation must not enable KA delegation")
	}
	off := &agent.DomainAgentProfile{ReactiveDelegation: &agent.ReactiveDelegationConfig{KnowledgeAgent: false}}
	if got := effectiveDelegations(off, nil); hasKA(got) {
		t.Error("knowledge_agent:false must not enable KA delegation")
	}

	// Opt-in via config → KA delegation present with the canonical descriptor.
	on := &agent.DomainAgentProfile{ReactiveDelegation: &agent.ReactiveDelegationConfig{KnowledgeAgent: true}}
	got := effectiveDelegations(on, nil)
	if !hasKA(got) {
		t.Fatal("knowledge_agent:true must enable KA delegation")
	}
	for _, d := range got {
		if d.ToolName == "ask_knowledge_agent" {
			if d.AgentName != "knowledge-agent" || d.Description == "" {
				t.Errorf("KA delegation descriptor malformed: %+v", d)
			}
		}
	}

	// Explicit custom delegations are preserved and merged.
	custom := []AgentDelegation{{ToolName: "ask_analytics", AgentName: "analytics-agent", Description: "Ask analytics"}}
	merged := effectiveDelegations(on, custom)
	if !hasKA(merged) || !func() bool {
		for _, d := range merged {
			if d.ToolName == "ask_analytics" {
				return true
			}
		}
		return false
	}() {
		t.Errorf("expected both custom and KA delegations, got %+v", merged)
	}

	// Dedup: an explicit ask_knowledge_agent option is not duplicated by config.
	explicitKA := []AgentDelegation{{ToolName: "ask_knowledge_agent", AgentName: "knowledge-agent", Description: "explicit"}}
	deduped := effectiveDelegations(on, explicitKA)
	count := 0
	for _, d := range deduped {
		if d.ToolName == "ask_knowledge_agent" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one ask_knowledge_agent delegation after dedup, got %d", count)
	}
}
