package mcptools

import (
	"encoding/json"
	"testing"
)

func TestDecodeEntity_Shapes(t *testing.T) {
	cases := map[string]string{
		"direct":       `{"id":"e1","name":"A"}`,
		"entity-wrap":  `{"entity":{"id":"e1","name":"A"}}`,
		"mcp-envelope": `{"content":[{"type":"text","text":"{\"id\":\"e1\",\"name\":\"A\"}"}]}`,
		"mcp+entity":   `{"content":[{"type":"text","text":"{\"entity\":{\"id\":\"e1\",\"name\":\"A\"}}"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			e := DecodeEntity(json.RawMessage(raw))
			if MapString(e, "id") != "e1" || MapString(e, "name") != "A" {
				t.Errorf("%s: decoded = %v", name, e)
			}
		})
	}

	if got := DecodeEntity(nil); got == nil || len(got) != 0 {
		t.Errorf("nil input should yield empty non-nil map, got %v", got)
	}
	if got := DecodeEntity(json.RawMessage(`{bad`)); got == nil || len(got) != 0 {
		t.Errorf("undecodable input should yield empty non-nil map, got %v", got)
	}
}

func TestDecodeEntities_Shapes(t *testing.T) {
	cases := map[string]string{
		"array":        `[{"id":"a"},{"id":"b"}]`,
		"entities":     `{"entities":[{"id":"a"},{"id":"b"}]}`,
		"results":      `{"results":[{"id":"a"},{"id":"b"}]}`,
		"items":        `{"items":[{"id":"a"},{"id":"b"}]}`,
		"mcp-envelope": `{"content":[{"type":"text","text":"{\"entities\":[{\"id\":\"a\"},{\"id\":\"b\"}]}"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			es := DecodeEntities(json.RawMessage(raw))
			if len(es) != 2 || MapString(es[0], "id") != "a" || MapString(es[1], "id") != "b" {
				t.Errorf("%s: decoded = %v", name, es)
			}
		})
	}
	if got := DecodeEntities(nil); got != nil {
		t.Errorf("nil input should yield nil, got %v", got)
	}
}

func TestMapGetters(t *testing.T) {
	m := map[string]any{
		"s":     "hi",
		"f":     3.5,
		"i":     float64(7),
		"b":     true,
		"slice": []any{"x", "y"},
		"properties": map[string]any{
			"nested": "deep",
			"ni":     float64(9),
		},
	}
	if MapString(m, "s") != "hi" {
		t.Error("MapString direct")
	}
	if MapString(m, "nested") != "deep" {
		t.Error("MapString via properties")
	}
	if MapFloat(m, "f") != 3.5 {
		t.Error("MapFloat")
	}
	if MapInt(m, "i") != 7 {
		t.Error("MapInt")
	}
	if MapInt(m, "ni") != 9 {
		t.Error("MapInt via properties")
	}
	if !MapBool(m, "b") {
		t.Error("MapBool")
	}
	if got := MapStringSlice(m, "slice"); len(got) != 2 || got[0] != "x" {
		t.Errorf("MapStringSlice = %v", got)
	}
	// Absent keys.
	if MapString(m, "nope") != "" || MapFloat(m, "nope") != 0 || MapBool(m, "nope") || MapStringSlice(m, "nope") != nil {
		t.Error("absent keys should yield zero values")
	}
	// nil map safety.
	if MapString(nil, "x") != "" || MapFloat(nil, "x") != 0 || MapBool(nil, "x") {
		t.Error("nil map should be safe")
	}
}

func TestOptBoolAndDedupAndSort(t *testing.T) {
	tr := true
	if !OptBool(&tr, false) {
		t.Error("OptBool with ptr")
	}
	if !OptBool(nil, true) || OptBool(nil, false) {
		t.Error("OptBool nil → fallback")
	}

	got := DedupSortedStrings([]string{"a", "a", "b", "b", "b", "c"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("DedupSortedStrings = %v", got)
	}

	entities := []map[string]any{{"id": "c"}, {"id": "a"}, {"id": "b"}}
	SortEntitiesByID(entities)
	if MapString(entities[0], "id") != "a" || MapString(entities[2], "id") != "c" {
		t.Errorf("SortEntitiesByID = %v", entities)
	}
}
