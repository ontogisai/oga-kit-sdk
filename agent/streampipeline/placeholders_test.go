package streampipeline

import (
	"context"
	"testing"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
)

func testEvent() *agent.ProactiveEvent {
	return &agent.ProactiveEvent{
		EventID:    "evt-001",
		EventType:  "EntityAnomalyEvent",
		EntityID:   "chiller-01",
		EntityType: "brick_Chiller",
		TenantID:   "sgac1",
		H3Cell:     "8a2a1072b59ffff",
		Severity:   "high",
		Payload: map[string]any{
			"manufacturer": "Carrier",
			"model":        "19XR",
			"capacity_rt":  float64(500),
			"online":       true,
		},
	}
}

func TestEventPlaceholderResolver_StaticTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	resolve := NewEventPlaceholderResolver(testEvent(), now)

	cases := map[string]string{
		"entity_id":   "chiller-01",
		"entity_type": "brick_Chiller",
		"event_type":  "EntityAnomalyEvent",
		"event_id":    "evt-001",
		"severity":    "high",
		"h3_cell":     "8a2a1072b59ffff",
		"tenant_id":   "sgac1",
		"time_now":    "2026-06-14T10:00:00Z",
	}
	for token, want := range cases {
		got, ok := resolve(token)
		if !ok {
			t.Errorf("token %q: ok=false, want resolvable", token)
		}
		if got != want {
			t.Errorf("token %q = %q, want %q", token, got, want)
		}
	}
}

func TestEventPlaceholderResolver_TimeMinus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	resolve := NewEventPlaceholderResolver(testEvent(), now)

	got, ok := resolve("time_minus_24h")
	if !ok {
		t.Fatal("time_minus_24h not resolved")
	}
	if got != "2026-06-13T10:00:00Z" {
		t.Errorf("time_minus_24h = %q, want 2026-06-13T10:00:00Z", got)
	}

	got, ok = resolve("time_minus_1h")
	if !ok {
		t.Fatal("time_minus_1h not resolved")
	}
	if got != "2026-06-14T09:00:00Z" {
		t.Errorf("time_minus_1h = %q, want 2026-06-14T09:00:00Z", got)
	}

	// Unparseable duration → not resolved.
	if _, ok := resolve("time_minus_banana"); ok {
		t.Error("time_minus_banana should not resolve")
	}
}

func TestEventPlaceholderResolver_EntityProperties(t *testing.T) {
	t.Parallel()
	resolve := NewEventPlaceholderResolver(testEvent(), time.Now())

	cases := map[string]string{
		"entity_properties.manufacturer": "Carrier",
		"entity_properties.model":        "19XR",
		"entity_properties.capacity_rt":  "500", // float64 integral → no decimal
		"entity_properties.online":       "true",
	}
	for token, want := range cases {
		got, ok := resolve(token)
		if !ok {
			t.Errorf("token %q: ok=false, want resolvable", token)
		}
		if got != want {
			t.Errorf("token %q = %q, want %q", token, got, want)
		}
	}

	// Missing property → not resolved.
	if _, ok := resolve("entity_properties.nonexistent"); ok {
		t.Error("missing entity property should not resolve")
	}
}

func TestEventPlaceholderResolver_UnknownAndNil(t *testing.T) {
	t.Parallel()
	resolve := NewEventPlaceholderResolver(testEvent(), time.Now())
	if _, ok := resolve("totally_unknown"); ok {
		t.Error("unknown token should not resolve")
	}

	nilResolve := NewEventPlaceholderResolver(nil, time.Now())
	if _, ok := nilResolve("entity_id"); ok {
		t.Error("nil event should resolve nothing")
	}
}

func TestSubstitutePlan_ResolvesAndPreservesProfileMap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	resolve := NewEventPlaceholderResolver(testEvent(), now)

	// Shared map mimicking a profile's grounding-step arguments.
	shared := map[string]any{"entity_id": "{entity_id}"}
	plan := &ToolPlan{Steps: []ToolPlanStep{
		{Name: "trigger_entity", ToolName: "kg_get_entity", Arguments: shared},
		{Name: "history", ToolName: "kg_query_entities", Arguments: map[string]any{
			"entity_type": "WorkOrder",
			"query":       "{entity_type} since {time_minus_24h}",
		}},
	}}

	substitutePlan(context.Background(), plan, resolve, nil)

	if got := plan.Steps[0].Arguments["entity_id"]; got != "chiller-01" {
		t.Errorf("step0 entity_id = %v, want chiller-01", got)
	}
	if got := plan.Steps[1].Arguments["query"]; got != "brick_Chiller since 2026-06-13T10:00:00Z" {
		t.Errorf("step1 query = %v, want substituted", got)
	}

	// The original shared profile map must NOT be mutated.
	if shared["entity_id"] != "{entity_id}" {
		t.Errorf("shared profile map was mutated: entity_id = %v, want {entity_id}", shared["entity_id"])
	}
}

func TestSubstitutePlan_NilResolverIsNoOp(t *testing.T) {
	t.Parallel()
	plan := &ToolPlan{Steps: []ToolPlanStep{
		{ToolName: "kg_get_entity", Arguments: map[string]any{"entity_id": "{entity_id}"}},
	}}
	substitutePlan(context.Background(), plan, nil, nil)
	if got := plan.Steps[0].Arguments["entity_id"]; got != "{entity_id}" {
		t.Errorf("nil resolver should be a no-op, got %v", got)
	}
}

func TestSubstitutePlan_UnknownTokenLeftVerbatim(t *testing.T) {
	t.Parallel()
	resolve := NewEventPlaceholderResolver(testEvent(), time.Now())
	plan := &ToolPlan{Steps: []ToolPlanStep{
		{ToolName: "kg_get_entity", Arguments: map[string]any{
			"a": "{entity_id}",
			"b": "{mystery_token}",
		}},
	}}
	substitutePlan(context.Background(), plan, resolve, nil)
	if plan.Steps[0].Arguments["a"] != "chiller-01" {
		t.Errorf("known token should resolve, got %v", plan.Steps[0].Arguments["a"])
	}
	if plan.Steps[0].Arguments["b"] != "{mystery_token}" {
		t.Errorf("unknown token should be left verbatim, got %v", plan.Steps[0].Arguments["b"])
	}
}

func TestSubstituteValue_NestedContainers(t *testing.T) {
	t.Parallel()
	resolve := NewEventPlaceholderResolver(testEvent(), time.Now())
	unresolved := make(map[string]struct{})
	in := map[string]any{
		"filter": map[string]any{"id": "{entity_id}"},
		"tags":   []any{"{entity_type}", "static"},
		"count":  float64(3), // non-string scalar preserved
	}
	out := substituteValue(in, resolve, unresolved).(map[string]any)

	if got := out["filter"].(map[string]any)["id"]; got != "chiller-01" {
		t.Errorf("nested map not substituted, got %v", got)
	}
	tags := out["tags"].([]any)
	if tags[0] != "brick_Chiller" || tags[1] != "static" {
		t.Errorf("slice not substituted correctly, got %v", tags)
	}
	if out["count"] != float64(3) {
		t.Errorf("non-string scalar should be preserved, got %v", out["count"])
	}
	if len(unresolved) != 0 {
		t.Errorf("expected no unresolved tokens, got %v", unresolved)
	}
}
