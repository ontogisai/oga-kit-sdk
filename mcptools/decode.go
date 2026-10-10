package mcptools

import (
	"encoding/json"
	"errors"
	"sort"
)

// ErrInvalidArgument is the canonical sentinel a tool handler wraps when the
// caller supplied invalid arguments. The runtime surfaces a returned error as
// an MCP tools/call result with isError=true; wrapping this sentinel gives
// every kit consistent "bad input" semantics:
//
//	if input.BuildingID == "" {
//		return nil, fmt.Errorf("%w: building_id is required", mcptools.ErrInvalidArgument)
//	}
var ErrInvalidArgument = errors.New("invalid argument")

// DecodeEntity decodes a single entity object from a platform tool's
// (kg_get_entity, kg_create_entity, …) gateway response. It transparently
// unwraps the two shapes the gateway returns:
//
//   - {"entity": {...}}
//   - the MCP content envelope {"content":[{"type":"text","text":"<json>"}]}
//     whose inner JSON is itself one of these shapes.
//
// It returns an empty (non-nil) map when the response can't be decoded, so
// handlers can read fields defensively rather than nil-checking.
func DecodeEntity(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	if inner, ok := m["entity"].(map[string]any); ok {
		return inner
	}
	if text, ok := mcpEnvelopeText(m); ok {
		var inner map[string]any
		if err := json.Unmarshal([]byte(text), &inner); err == nil {
			if e, ok := inner["entity"].(map[string]any); ok {
				return e
			}
			return inner
		}
	}
	return m
}

// DecodeEntities decodes a list of entity objects from a platform tool's
// gateway response. It accepts a direct array, the common wrapper keys
// ({"entities"|"results"|"items"|"data"|"rows": [...]}), and the MCP content
// envelope whose inner JSON matches any of those shapes. Returns nil when
// nothing decodes.
func DecodeEntities(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var wrapper map[string]any
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil
	}
	for _, key := range []string{"entities", "results", "items", "data", "rows"} {
		if v, ok := wrapper[key]; ok {
			return toEntitySlice(v)
		}
	}
	if text, ok := mcpEnvelopeText(wrapper); ok {
		return DecodeEntities(json.RawMessage(text))
	}
	return nil
}

// mcpEnvelopeText returns the first content part's text from an MCP tools/call
// content envelope ({"content":[{"type":"text","text":"…"}]}), or ("", false).
func mcpEnvelopeText(m map[string]any) (string, bool) {
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		return "", false
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return "", false
	}
	text, ok := first["text"].(string)
	return text, ok
}

func toEntitySlice(v any) []map[string]any {
	rawArr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(rawArr))
	for _, item := range rawArr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// MapString reads a string property from an entity map, falling back to a
// nested "properties" sub-map (the platform entity shape). Returns "" when
// absent or not a string.
func MapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	if props, ok := m["properties"].(map[string]any); ok {
		if s, ok := props[key].(string); ok {
			return s
		}
	}
	return ""
}

// MapFloat reads a numeric property as float64 (handles the float/int variants
// JSON unmarshal produces), falling back to the "properties" sub-map. Returns
// 0 when absent.
func MapFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		return toFloat(v)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		if v, ok := props[key]; ok {
			return toFloat(v)
		}
	}
	return 0
}

// MapInt reads a numeric property as int. Returns 0 when absent.
func MapInt(m map[string]any, key string) int {
	return int(MapFloat(m, key))
}

// MapBool reads a bool property, falling back to the "properties" sub-map.
// Returns false when absent or not a bool.
func MapBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if b, ok := m[key].(bool); ok {
		return b
	}
	if props, ok := m["properties"].(map[string]any); ok {
		if b, ok := props[key].(bool); ok {
			return b
		}
	}
	return false
}

// MapStringSlice reads a []string property, falling back to the "properties"
// sub-map. Returns nil when absent or not an array of strings.
func MapStringSlice(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		if props, ok := m["properties"].(map[string]any); ok {
			v = props[key]
		}
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	}
	return 0
}

// OptBool returns the value of an optional bool pointer, or fallback when nil.
// Useful for tool arguments with a schema default.
func OptBool(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// SortEntitiesByID sorts entities by their "id" field in place, for
// deterministic tool output (required for audit-stable Tier 3 results).
func SortEntitiesByID(entities []map[string]any) {
	sort.SliceStable(entities, func(i, j int) bool {
		return MapString(entities[i], "id") < MapString(entities[j], "id")
	})
}

// DedupSortedStrings removes adjacent duplicates from a sorted slice in place.
func DedupSortedStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
