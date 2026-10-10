package agent

import (
	"reflect"
	"testing"
)

func TestResolveToolPatterns(t *testing.T) {
	avail := []string{"kg_search", "kg_query_entities", "kg_doc_content", "kg_doc_search", "kg_traverse"}

	tests := []struct {
		name     string
		patterns []string
		avail    []string
		want     []string
	}{
		{
			name:     "exact names filtered by catalog",
			patterns: []string{"kg_search", "kg_missing"},
			avail:    avail,
			want:     []string{"kg_search"},
		},
		{
			name:     "prefix glob",
			patterns: []string{"kg_doc_*"},
			avail:    avail,
			want:     []string{"kg_doc_content", "kg_doc_search"},
		},
		{
			name:     "suffix glob",
			patterns: []string{"*_search"},
			avail:    avail,
			want:     []string{"kg_search", "kg_doc_search"},
		},
		{
			name:     "catch-all glob",
			patterns: []string{"kg_*"},
			avail:    avail,
			want:     []string{"kg_search", "kg_query_entities", "kg_doc_content", "kg_doc_search", "kg_traverse"},
		},
		{
			name:     "exact + glob deduped",
			patterns: []string{"kg_search", "kg_doc_*"},
			avail:    avail,
			want:     []string{"kg_search", "kg_doc_content", "kg_doc_search"},
		},
		{
			name:     "exact name passes through with no catalog",
			patterns: []string{"kg_search", "kg_traverse"},
			avail:    nil,
			want:     []string{"kg_search", "kg_traverse"},
		},
		{
			name:     "glob yields nothing with no catalog",
			patterns: []string{"kg_*"},
			avail:    nil,
			want:     nil,
		},
		{
			name:     "empty patterns",
			patterns: nil,
			avail:    avail,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveToolPatterns(tt.patterns, tt.avail)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResolveToolPatterns(%v, %v) = %v, want %v", tt.patterns, tt.avail, got, tt.want)
			}
		})
	}
}

func TestResolveProfileTools_GlobExpansion(t *testing.T) {
	profile := &DomainAgentProfile{
		Capabilities: []CapabilityDef{
			{Name: "kg", Tools: []string{"kg_*"}},
			{Name: "fm", Tools: []string{"create_work_order"}},
		},
	}
	avail := []string{"kg_search", "kg_traverse", "create_work_order", "zone_access_check"}

	got := ResolveProfileTools(profile, avail)
	want := []string{"kg_search", "kg_traverse", "create_work_order"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveProfileTools = %v, want %v", got, want)
	}
}

func TestResolveProfileTools_NoCatalogFallsBackToDeclared(t *testing.T) {
	profile := &DomainAgentProfile{
		Capabilities: []CapabilityDef{
			{Name: "kg", Tools: []string{"kg_search", "kg_traverse"}},
		},
	}
	got := ResolveProfileTools(profile, nil)
	want := []string{"kg_search", "kg_traverse"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no-catalog fallback = %v, want %v", got, want)
	}
}
