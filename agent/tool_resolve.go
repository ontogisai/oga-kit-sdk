// Package agent — tool pattern resolution (OGA-419).
//
// Domain kits may declare capability tools using glob patterns (e.g. "kg_*",
// "*_search") to avoid enumerating every dynamically-registered tool by hand.
// The planner, however, needs a CONCRETE list of tool names to render into the
// decision prompt (the LLM picks from it). ResolveToolPatterns bridges the two:
// it expands the kit-declared patterns against the live tool catalog discovered
// from the gateway, producing the concrete palette handed to
// streampipeline.PlannerPersona.Tools. Keeping resolution at the catalog layer
// (not in the persona) means the persona stays a plain []string the model can
// choose from.
package agent

import (
	"path"
	"strings"
)

// ResolveToolPatterns expands tool patterns against the set of available tool
// names and returns the concrete matched set, deduplicated and in a stable
// order (exact patterns in declaration order first, then glob matches in
// `available` order).
//
// A pattern may be:
//   - an exact tool name ("kg_search") — included iff present in `available`
//     (or passed through unchanged when `available` is empty: the caller has no
//     catalog to filter against);
//   - a glob ("kg_*", "*_search", "kg_doc_?") using path.Match semantics —
//     expanded to every matching name in `available`. A glob never passes
//     through unmatched (a glob with no catalog yields nothing for that
//     pattern).
func ResolveToolPatterns(patterns, available []string) []string {
	seen := make(map[string]struct{}, len(available))
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	availSet := make(map[string]struct{}, len(available))
	for _, a := range available {
		availSet[a] = struct{}{}
	}

	for _, p := range patterns {
		if !isGlobPattern(p) {
			// Exact name. With no catalog, pass through; with a catalog, keep
			// only if it actually exists (drops stale/typo'd declarations).
			if len(available) == 0 {
				add(p)
			} else if _, ok := availSet[p]; ok {
				add(p)
			}
			continue
		}
		// Glob: match against the live catalog only.
		for _, a := range available {
			if ok, err := path.Match(p, a); err == nil && ok {
				add(a)
			}
		}
	}
	return out
}

// ResolveProfileTools expands a domain agent profile's declared capability
// tools (which may contain globs) against the available tool catalog. When the
// catalog is empty (no discovery yet), it falls back to the declared names
// verbatim via UniqueTools — so a no-catalog path is never worse than today.
func ResolveProfileTools(profile *DomainAgentProfile, available []string) []string {
	declared := UniqueTools(profile)
	if len(available) == 0 {
		return declared
	}
	return ResolveToolPatterns(declared, available)
}

// isGlobPattern reports whether s contains path.Match wildcard metacharacters.
func isGlobPattern(s string) bool {
	return strings.ContainsAny(s, "*?[")
}
