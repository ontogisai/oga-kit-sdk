package agent

import (
	"encoding/json"
	"strings"
)

// resolveDependentArgs enriches the arguments of a dependent step with values
// extracted from the prior step's result. This bridges the gap between the LLM
// planner (which uses placeholders like "<from step 0>") and the MCP tool
// handlers (which expect resolved values like a UUID entity_id).
//
// The function:
//  1. Always injects "_prior_result" with the raw prior content (for tools that
//     parse it directly).
//  2. Resolves placeholder arguments: when a string value starts with "<from step"
//     or "<from prior", the function replaces it with the corresponding value
//     extracted from the prior result.
//  3. Auto-resolves entity_id / start_entity_id / source_id: when empty or
//     placeholder, extracts from the prior result's "results" array.
func resolveDependentArgs(args map[string]any, priorContent string) map[string]any {
	if args == nil {
		args = make(map[string]any)
	}

	// Always inject the raw prior result for tools that want to parse it themselves.
	args["_prior_result"] = priorContent

	// Parse the prior result to extract structured values.
	extracted := extractPriorValues(priorContent)
	if extracted == nil {
		return args
	}

	// Resolve placeholder arguments from the prior result.
	for key, val := range args {
		if key == "_prior_result" {
			continue
		}
		strVal, ok := val.(string)
		if !ok {
			continue
		}
		if isPlaceholder(strVal) {
			resolved := resolveFromExtracted(key, extracted)
			if resolved != "" {
				args[key] = resolved
			}
		}
	}

	// Auto-resolve: if entity_id is empty/missing and we extracted one, inject it.
	if entityID, hasField := args["entity_id"]; !hasField || entityID == "" || isPlaceholder(entityIDString(entityID)) {
		if extracted.firstEntityID != "" {
			args["entity_id"] = extracted.firstEntityID
		}
	}

	// Auto-resolve: if start_entity_id is empty/placeholder, inject it.
	if startID, hasField := args["start_entity_id"]; !hasField || startID == "" || isPlaceholder(entityIDString(startID)) {
		if extracted.firstEntityID != "" {
			args["start_entity_id"] = extracted.firstEntityID
		}
	}

	// Auto-resolve: if source_id is empty/placeholder (for time-series tools), inject it.
	if sourceID, hasField := args["source_id"]; !hasField || sourceID == "" || isPlaceholder(entityIDString(sourceID)) {
		if extracted.firstEntityID != "" {
			args["source_id"] = extracted.firstEntityID
		}
	}

	return args
}

// extractedValues holds values parsed from a prior step result.
type extractedValues struct {
	firstEntityID string
	entityIDs     []string
	entityType    string
	label         string
}

// extractPriorValues parses the prior result JSON and extracts commonly needed
// values. Handles: {"results":[{"entity_id":...}]} and {"entity_id":...}.
func extractPriorValues(content string) *extractedValues {
	if content == "" {
		return nil
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return nil
	}

	ev := &extractedValues{}

	// Shape 1: {"results": [...]} — search/query responses
	if results, ok := obj["results"]; ok {
		if arr, ok := results.([]any); ok && len(arr) > 0 {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					if id := stringField(m, "entity_id"); id != "" {
						ev.entityIDs = append(ev.entityIDs, id)
						if ev.firstEntityID == "" {
							ev.firstEntityID = id
						}
					}
					if ev.entityType == "" {
						ev.entityType = stringField(m, "entity_type")
					}
					if ev.label == "" {
						ev.label = stringField(m, "label")
					}
				}
			}
			return ev
		}
	}

	// Shape 2: {"entity_id": "..."} — single entity response
	if id := stringField(obj, "entity_id"); id != "" {
		ev.firstEntityID = id
		ev.entityIDs = []string{id}
		ev.entityType = stringField(obj, "entity_type")
		ev.label = stringField(obj, "label")
		return ev
	}

	// Shape 3: {"id": "..."} — alternative single entity
	if id := stringField(obj, "id"); id != "" {
		ev.firstEntityID = id
		ev.entityIDs = []string{id}
		ev.entityType = stringField(obj, "entity_type")
		ev.label = stringField(obj, "label")
		return ev
	}

	return ev
}

// resolveFromExtracted maps a specific argument key to the corresponding
// extracted value.
func resolveFromExtracted(key string, ev *extractedValues) string {
	switch key {
	case "entity_id", "start_entity_id", "source_id", "document_id",
		"source_entity_id", "target_entity_id":
		return ev.firstEntityID
	case "entity_type":
		return ev.entityType
	case "label":
		return ev.label
	default:
		return ""
	}
}

// isPlaceholder returns true if a string value looks like an LLM-generated
// placeholder that needs resolution (e.g., "<from step 0>", "<from prior>").
func isPlaceholder(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "<from step") ||
		strings.HasPrefix(lower, "<from prior") ||
		strings.HasPrefix(lower, "<result from") ||
		strings.HasPrefix(lower, "<entity_id from") ||
		strings.HasPrefix(lower, "<id from") ||
		(strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"))
}

// stringField extracts a string field from a map, returning "" if not present or wrong type.
func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// entityIDString safely converts an interface to string for placeholder checking.
func entityIDString(v any) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
