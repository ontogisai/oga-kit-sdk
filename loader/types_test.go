package loader_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
)

func TestLoaderKind_IsValid(t *testing.T) {
	tests := []struct {
		name string
		kind loader.LoaderKind
		want bool
	}{
		{"ontology", loader.KindOntology, true},
		{"data", loader.KindData, true},
		{"empty", loader.LoaderKind(""), false},
		{"unknown", loader.LoaderKind("widget"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.IsValid(); got != tt.want {
				t.Errorf("LoaderKind(%q).IsValid() = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestLoaderKind_OrDefault(t *testing.T) {
	tests := []struct {
		name string
		kind loader.LoaderKind
		want loader.LoaderKind
	}{
		{"explicit ontology", loader.KindOntology, loader.KindOntology},
		{"explicit data", loader.KindData, loader.KindData},
		{"empty falls back to data", loader.LoaderKind(""), loader.KindData},
		{"unknown is preserved", loader.LoaderKind("widget"), loader.LoaderKind("widget")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.OrDefault(); got != tt.want {
				t.Errorf("LoaderKind(%q).OrDefault() = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

// TestLoadStats_EdgeCorrectionRetirementRevivalFields_JSONRoundTrip pins the
// wire shape of the edge-topology-repair fields on LoadStats
// (edges_corrected, edges_retired, edges_revived), added so a loader that
// tracks these distinctions (e.g. a bi-temporal-edge-write repository that
// distinguishes a topology repair, a cardinality-capped re-parent
// retirement, and a re-parent that cycled back to a prior target) can report
// them to the platform without overloading edges_updated. All three are
// additive and optional (omitempty), so an existing loader response that
// omits them decodes identically to before this change.
func TestLoadStats_EdgeCorrectionRetirementRevivalFields_JSONRoundTrip(t *testing.T) {
	stats := loader.LoadStats{
		EdgesCreated:   1,
		EdgesUpdated:   2,
		EdgesCorrected: 3,
		EdgesRetired:   4,
		EdgesRevived:   5,
	}

	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	wantSubstrings := []string{
		`"edges_corrected":3`,
		`"edges_retired":4`,
		`"edges_revived":5`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled LoadStats = %s, want to contain %q", data, want)
		}
	}

	var decoded loader.LoadStats
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.EdgesCreated != stats.EdgesCreated ||
		decoded.EdgesUpdated != stats.EdgesUpdated ||
		decoded.EdgesCorrected != stats.EdgesCorrected ||
		decoded.EdgesRetired != stats.EdgesRetired ||
		decoded.EdgesRevived != stats.EdgesRevived {
		t.Errorf("round-tripped LoadStats = %+v, want %+v", decoded, stats)
	}
}

// TestLoadStats_EdgeCorrectionRetirementRevivalFields_OmittedWhenZero pins
// that a loader response omitting these fields entirely (the pre-existing
// wire shape, still produced by loaders that predate this change) decodes to
// their zero value rather than requiring every loader to be updated.
func TestLoadStats_EdgeCorrectionRetirementRevivalFields_OmittedWhenZero(t *testing.T) {
	body := `{"vertices_created": 1, "edges_created": 1}`

	var decoded loader.LoadStats
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.EdgesCorrected != 0 || decoded.EdgesRetired != 0 || decoded.EdgesRevived != 0 {
		t.Errorf("decoded = %+v, want EdgesCorrected/EdgesRetired/EdgesRevived all 0", decoded)
	}

	data, err := json.Marshal(loader.LoadStats{VerticesCreated: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, absent := range []string{"edges_corrected", "edges_retired", "edges_revived"} {
		if strings.Contains(string(data), absent) {
			t.Errorf("marshaled zero-value LoadStats = %s, want %q omitted", data, absent)
		}
	}
}
