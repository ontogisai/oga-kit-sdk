package agent

import (
	"encoding/json"
	"sort"
	"strings"
)

// priorResultKey is the args map key under which the raw upstream tool output
// is injected, for handlers that re-parse it directly.
const priorResultKey = "_prior_result"

// IDKind classifies an extracted identifier so it fills the right argument
// fields. An entity identifier never fills a document argument and vice versa.
type IDKind string

const (
	IDKindEntity       IDKind = "entity"
	IDKindDocument     IDKind = "document"
	IDKindRelationship IDKind = "relationship"
)

// Identifier is one identifier extracted from a prior step's result, plus its
// semantic kind and (when available) the entity type / label that accompanied
// it. EntityType and Label support resolving `entity_type` / `label`
// placeholder arguments and building citations without re-parsing.
type Identifier struct {
	Value      string
	Kind       IDKind
	EntityType string
	Label      string
}

// ShapeDescriptor describes how to extract identifiers from one producing
// tool's JSON output. See DefaultRegistry for the per-tool table.
type ShapeDescriptor struct {
	// ArrayKeys are candidate top-level array keys, tried in order. The first
	// key that yields at least one identifier wins.
	ArrayKeys []string
	// ItemIDFields are item-level identifier fields, tried in order; the first
	// non-empty field on an item is used.
	ItemIDFields []string
	// NestedPaths handle items whose IDs live below the item root. A 1-element
	// path names an array-of-strings field (e.g. ["matched_entities"]); a
	// 2-element path names an array-of-objects field plus the ID field within
	// each object (e.g. ["path", "entity_id"]).
	NestedPaths [][]string
	// SingleIDFields handle non-array single-object responses (e.g.
	// kg_get_entity → ["entity_id", "id"]).
	SingleIDFields []string
	// Kind is the ID kind every identifier extracted via this descriptor
	// carries (NestedPaths string-arrays are always entity kind).
	Kind IDKind
}

// Registry maps a producing tool name to its shape descriptor.
type Registry map[string]ShapeDescriptor

// ResolveDependentArgs resolves a dependent step's placeholder arguments from
// the prior step's result, with no knowledge of the producing tool. It always
// injects the raw prior content under "_prior_result". This is the
// backward-compatible entry point; it delegates to the heuristic path.
//
// Prefer ResolveDependentArgsForTool when the producing tool name is known —
// it uses the shape registry for precise extraction.
func ResolveDependentArgs(args map[string]any, priorContent string) map[string]any {
	return ResolveDependentArgsForTool(args, priorContent, "")
}

// ResolveDependentArgsForTool resolves placeholder arguments using the shape
// registry entry for producingTool. When producingTool is empty or unknown, or
// when the descriptor yields no identifier, it falls back to a bounded
// heuristic scan.
//
// The function:
//  1. Always injects "_prior_result" with the raw prior content.
//  2. Resolves named placeholder arguments (e.g. "start_entity_id":
//     "<from step 1>") from a same-kind identifier.
//  3. Auto-fills empty/missing/placeholder ID fields by kind (entity →
//     entity_id/start_entity_id/source_id; document → document_id).
//  4. Never overwrites a concrete (non-placeholder) value.
func ResolveDependentArgsForTool(args map[string]any, priorContent, producingTool string) map[string]any {
	if args == nil {
		args = make(map[string]any)
	}
	args[priorResultKey] = priorContent
	if priorContent == "" {
		return args
	}

	ids := Extract(priorContent, lookupDescriptor(producingTool))
	if len(ids) == 0 {
		return args
	}

	firstEntity := firstOfKind(ids, IDKindEntity)
	firstDocument := firstOfKind(ids, IDKindDocument)
	firstRelationship := firstOfKind(ids, IDKindRelationship)

	resolveNamedPlaceholders(args, firstEntity, firstDocument, firstRelationship)
	autoFillIDFields(args, firstEntity, firstDocument)

	return args
}

// resolveNamedPlaceholders replaces placeholder-valued arguments with a value
// of the matching kind. An unknown field with a placeholder value is left
// untouched.
func resolveNamedPlaceholders(args map[string]any, entity, document, relationship *Identifier) {
	for key, val := range args {
		if key == priorResultKey {
			continue
		}
		s, ok := val.(string)
		if !ok || !isPlaceholder(s) {
			continue
		}
		if resolved := resolveNamedField(key, entity, document, relationship); resolved != "" {
			args[key] = resolved
		}
	}
}

// resolveNamedField maps an argument field to a value of the field's kind.
func resolveNamedField(field string, entity, document, relationship *Identifier) string {
	switch field {
	case "entity_id", "start_entity_id", "source_id",
		"source_entity_id", "target_entity_id":
		if entity != nil {
			return entity.Value
		}
	case "document_id":
		if document != nil {
			return document.Value
		}
	case "relationship_id":
		if relationship != nil {
			return relationship.Value
		}
	case "entity_type":
		if entity != nil {
			return entity.EntityType
		}
	case "label":
		if entity != nil {
			return entity.Label
		}
	}
	return ""
}

// autoFillIDFields fills well-known ID fields that are missing, empty, or hold
// a placeholder. Entity IDs fill entity fields only; document IDs fill
// document_id only (Requirement 4 — kind safety).
func autoFillIDFields(args map[string]any, entity, document *Identifier) {
	if entity != nil {
		for _, field := range []string{"entity_id", "start_entity_id", "source_id"} {
			if needsResolution(args, field) {
				args[field] = entity.Value
			}
		}
	}
	if document != nil && needsResolution(args, "document_id") {
		args["document_id"] = document.Value
	}
}

// needsResolution reports whether a field is absent, empty, or holds a placeholder.
func needsResolution(args map[string]any, field string) bool {
	val, ok := args[field]
	if !ok {
		return true
	}
	s, isStr := val.(string)
	if !isStr {
		return false
	}
	return s == "" || isPlaceholder(s)
}

// firstOfKind returns the first identifier of the given kind in document order,
// or nil when none is present.
func firstOfKind(ids []Identifier, kind IDKind) *Identifier {
	for i := range ids {
		if ids[i].Kind == kind {
			return &ids[i]
		}
	}
	return nil
}

// Extract returns every identifier found in priorContent, in document order.
// When d is non-nil, descriptor-driven extraction is tried first; if it yields
// nothing (or d is nil), the bounded heuristic scan runs. Returns nil for empty
// or unparseable content.
func Extract(priorContent string, d *ShapeDescriptor) []Identifier {
	if priorContent == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(priorContent), &obj); err != nil {
		return nil
	}
	if d != nil {
		if ids := extractWithDescriptor(obj, *d); len(ids) > 0 {
			return ids
		}
	}
	return heuristicScan(obj)
}

// extractWithDescriptor applies a registry descriptor to a decoded object.
func extractWithDescriptor(obj map[string]any, d ShapeDescriptor) []Identifier {
	var ids []Identifier

	// Single-object responses (e.g. kg_get_entity).
	for _, f := range d.SingleIDFields {
		if v := stringField(obj, f); v != "" {
			ids = append(ids, identifierFromMap(obj, v, d.Kind))
			break
		}
	}

	// Array responses. First array key that yields identifiers wins.
	for _, key := range d.ArrayKeys {
		arr, ok := obj[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		var keyIDs []Identifier
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if v := firstNonEmptyField(m, d.ItemIDFields); v != "" {
				keyIDs = append(keyIDs, identifierFromMap(m, v, d.Kind))
			}
			keyIDs = append(keyIDs, extractNested(m, d.NestedPaths, d.Kind)...)
		}
		if len(keyIDs) > 0 {
			ids = append(ids, keyIDs...)
			break
		}
	}

	return ids
}

// extractNested pulls identifiers from item-relative nested paths.
func extractNested(item map[string]any, paths [][]string, kind IDKind) []Identifier {
	var ids []Identifier
	for _, path := range paths {
		switch len(path) {
		case 1:
			// Array of bare ID strings (e.g. matched_entities).
			if arr, ok := item[path[0]].([]any); ok {
				for _, e := range arr {
					if s, ok := e.(string); ok && identifierShaped(s) {
						ids = append(ids, Identifier{Value: s, Kind: kind})
					}
				}
			}
		case 2:
			// Array of objects; read path[1] from each (e.g. path[].entity_id).
			if arr, ok := item[path[0]].([]any); ok {
				for _, e := range arr {
					if m, ok := e.(map[string]any); ok {
						if v := stringField(m, path[1]); v != "" {
							ids = append(ids, identifierFromMap(m, v, kind))
						}
					}
				}
			}
		}
	}
	return ids
}

// identifierFromMap builds an Identifier, carrying entity_type/label when present.
func identifierFromMap(m map[string]any, value string, kind IDKind) Identifier {
	return Identifier{
		Value:      value,
		Kind:       kind,
		EntityType: stringField(m, "entity_type"),
		Label:      stringField(m, "label"),
	}
}

// firstNonEmptyField returns the first non-empty string field from fields.
func firstNonEmptyField(m map[string]any, fields []string) string {
	for _, f := range fields {
		if v := stringField(m, f); v != "" {
			return v
		}
	}
	return ""
}

// heuristicAllowFields is the allow-list of object keys the heuristic treats as
// identifiers, in fixed priority order. Keys NOT in this list (notably
// tenant_id and audit *_by fields) are never read as identifiers.
var heuristicAllowFields = []string{
	"entity_id", "id", "document_id", "source_id", "start_entity_id",
	"source_entity_id", "target_entity_id", "representative_entity",
}

// heuristicAllowArrayFields are object keys holding arrays of bare ID strings.
var heuristicAllowArrayFields = []string{"matched_entities"}

// heuristicArrayKeyPriority is the fixed order in which the heuristic looks for
// the first array-of-objects. A fixed order (rather than Go map iteration
// order) makes the scan deterministic — Correctness Property 6.
var heuristicArrayKeyPriority = []string{
	"results", "entities", "nodes", "documents", "passages",
	"relationships", "clusters", "items", "matches",
}

// heuristicScan is the bounded, allow-list fallback used when the producing
// tool is unknown or the descriptor yields nothing. It scans (depth ≤ 2):
//   - top-level scalar identifier fields,
//   - the first array-of-objects found by fixed key priority, and
//   - one level of nested arrays-of-objects within those items
//     (e.g. kg_reason results[].path[].entity_id).
//
// Identifiers are returned in document order (array element order is preserved
// by the JSON decoder; the array-key choice is deterministic).
func heuristicScan(obj map[string]any) []Identifier {
	var ids []Identifier

	// Depth 0: top-level scalar id fields.
	for _, k := range heuristicAllowFields {
		if s := stringField(obj, k); identifierShaped(s) {
			ids = append(ids, Identifier{Value: s, Kind: kindForField(k)})
		}
	}

	// Depth 1: the first array-of-objects by fixed key priority.
	arr := firstArrayOfObjects(obj)
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ids = append(ids, heuristicScanItem(m)...)
	}

	return ids
}

// heuristicScanItem scans one array item: its allow-listed scalar/array ID
// fields plus one level of nested arrays-of-objects.
func heuristicScanItem(m map[string]any) []Identifier {
	var ids []Identifier

	for _, f := range heuristicAllowFields {
		if s := stringField(m, f); identifierShaped(s) {
			ids = append(ids, identifierFromMap(m, s, kindForField(f)))
		}
	}
	for _, af := range heuristicAllowArrayFields {
		if arr, ok := m[af].([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok && identifierShaped(s) {
					ids = append(ids, Identifier{Value: s, Kind: IDKindEntity})
				}
			}
		}
	}

	// Depth 2: nested arrays-of-objects (e.g. path[].entity_id). Keys sorted
	// for determinism.
	for _, nk := range sortedKeys(m) {
		arr, ok := m[nk].([]any)
		if !ok {
			continue
		}
		for _, e := range arr {
			nm, ok := e.(map[string]any)
			if !ok {
				continue
			}
			for _, f := range heuristicAllowFields {
				if s := stringField(nm, f); identifierShaped(s) {
					ids = append(ids, identifierFromMap(nm, s, kindForField(f)))
				}
			}
		}
	}

	return ids
}

// firstArrayOfObjects returns the first array-of-objects found by fixed key
// priority, then by sorted key order for any remaining keys (deterministic).
func firstArrayOfObjects(obj map[string]any) []any {
	for _, k := range heuristicArrayKeyPriority {
		if arr, ok := obj[k].([]any); ok && len(arr) > 0 && isArrayOfObjects(arr) {
			return arr
		}
	}
	for _, k := range sortedKeys(obj) {
		if arr, ok := obj[k].([]any); ok && len(arr) > 0 && isArrayOfObjects(arr) {
			return arr
		}
	}
	return nil
}

// isArrayOfObjects reports whether the first element is a JSON object.
func isArrayOfObjects(arr []any) bool {
	if len(arr) == 0 {
		return false
	}
	_, ok := arr[0].(map[string]any)
	return ok
}

// kindForField maps an identifier field name to its ID kind.
func kindForField(field string) IDKind {
	if field == "document_id" {
		return IDKindDocument
	}
	return IDKindEntity
}

// identifierShaped reports whether s is acceptable as an identifier value:
// non-empty and not itself a placeholder. Non-UUID identifiers are accepted
// (allowNonUUID is ON) because the allow-list + placeholder guards already
// prevent false positives, and several platform ID schemes are not UUIDs.
func identifierShaped(s string) bool {
	return s != "" && !isPlaceholder(s)
}

// sortedKeys returns the map keys in sorted order (deterministic iteration).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isPlaceholder reports whether s looks like an LLM-generated placeholder that
// needs resolution (e.g. "<from step 0>", "<from prior>").
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

// stringField extracts a string field from a map, returning "" if absent or not
// a string.
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
