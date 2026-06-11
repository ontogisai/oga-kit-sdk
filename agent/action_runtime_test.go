package agent

import (
	"context"
	"testing"
)

func proactiveMsg(metadata map[string]any, body string) *A2AMessage {
	parts := []Part{}
	if body != "" {
		parts = append(parts, Part{Text: body})
	}
	return &A2AMessage{
		Method: "message/send",
		Params: &MessageParams{Message: &Message{Role: "user", Parts: parts, Metadata: metadata}},
	}
}

func TestReadIntent(t *testing.T) {
	if got := readIntent(map[string]any{"intent": "proactive_event"}); got != IntentProactiveEvent {
		t.Errorf("got %q, want %q", got, IntentProactiveEvent)
	}
	if got := readIntent(nil); got != "" {
		t.Errorf("nil metadata should yield empty, got %q", got)
	}
}

func TestParseProactiveEvent_FromMetadata(t *testing.T) {
	msg := proactiveMsg(map[string]any{
		"intent":      "proactive_event",
		"event_type":  "EntityAnomalyEvent",
		"entity_id":   "CH-01",
		"entity_type": "Chiller",
		"tenant_id":   "sgac1",
		"severity":    "high",
	}, "")
	ev, err := ParseProactiveEvent(msg)
	if err != nil {
		t.Fatalf("ParseProactiveEvent: %v", err)
	}
	if ev.EventType != "EntityAnomalyEvent" || ev.EntityID != "CH-01" || ev.Severity != "high" {
		t.Errorf("unexpected event: %+v", ev)
	}
	if ev.Timestamp.IsZero() {
		t.Error("timestamp should default to now")
	}
}

func TestParseProactiveEvent_FromJSONBody(t *testing.T) {
	body := `{"event_type":"EntityAnomalyEvent","entity_id":"CH-02","payload":{"metric":"temp"}}`
	ev, err := ParseProactiveEvent(proactiveMsg(map[string]any{}, body))
	if err != nil {
		t.Fatalf("ParseProactiveEvent: %v", err)
	}
	if ev.EntityID != "CH-02" || ev.Payload["metric"] != "temp" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestParseProactiveEvent_MissingFields(t *testing.T) {
	_, err := ParseProactiveEvent(proactiveMsg(map[string]any{"event_type": "X"}, ""))
	if err == nil {
		t.Fatal("expected error when entity_id missing")
	}
}

func TestProfile_CandidateActions(t *testing.T) {
	p := &DomainAgentProfile{
		Name: "fm-ops",
		ProactiveReasoning: &ProactiveConfig{
			Actions: []ActionDef{
				{Name: "always", HumanActionMode: "approval", RiskLevel: "low", Outcome: OutcomeDef{KnowledgeGraphEntity: &KnowledgeGraphEntityDef{Type: EntityTypeExisting, Name: "A"}}},
				{Name: "anomaly_only", HumanActionMode: "approval", RiskLevel: "low", Outcome: OutcomeDef{KnowledgeGraphEntity: &KnowledgeGraphEntityDef{Type: EntityTypeExisting, Name: "B"}},
					Triggers: []TriggerDef{{EventType: "EntityAnomalyEvent"}}},
				{Name: "other_only", HumanActionMode: "approval", RiskLevel: "low", Outcome: OutcomeDef{KnowledgeGraphEntity: &KnowledgeGraphEntityDef{Type: EntityTypeExisting, Name: "C"}},
					Triggers: []TriggerDef{{EventType: "SomethingElse"}}},
			},
		},
	}
	got := p.CandidateActions(&ProactiveEvent{EventType: "EntityAnomalyEvent"})
	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	if !names["always"] || !names["anomaly_only"] || names["other_only"] {
		t.Errorf("unexpected candidates: %v", names)
	}
	if _, ok := p.Action("anomaly_only"); !ok {
		t.Error("Action(anomaly_only) should resolve")
	}
	if _, ok := p.Action("missing"); ok {
		t.Error("Action(missing) should not resolve")
	}
}

func TestHandleMessage_MessageHandlerOverride(t *testing.T) {
	called := false
	rt := NewDefaultRuntime(&DomainAgentProfile{Name: "x", Port: "8200"}, &RuntimeDeps{},
		WithMessageHandler(func(_ context.Context, _ *DefaultRuntime, _ *A2AMessage) (*A2AResponse, error) {
			called = true
			return &A2AResponse{Message: &Message{Role: "agent", Parts: []Part{{Text: "override"}}}}, nil
		}),
	)
	resp, err := rt.HandleMessage(context.Background(), proactiveMsg(map[string]any{"intent": "proactive_event"}, ""))
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if !called || resp.Message.Parts[0].Text != "override" {
		t.Errorf("override handler not used: called=%v resp=%+v", called, resp)
	}
}

func TestHandleMessage_ProactiveFallbackAcks(t *testing.T) {
	rt := NewDefaultRuntime(&DomainAgentProfile{Name: "x", Port: "8200"}, &RuntimeDeps{})
	msg := proactiveMsg(map[string]any{
		"intent": "proactive_event", "event_type": "EntityAnomalyEvent", "entity_id": "CH-01",
	}, "")
	resp, err := rt.HandleMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if resp.Message == nil || resp.Message.Parts[0].Text == "" {
		t.Errorf("expected ack response, got %+v", resp)
	}
}
