package transfer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEdgeCompleteness_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      *EdgeCompleteness
		wantErr string
	}{
		{name: "nil is valid (no assertion)", in: nil},
		{name: "zero mode is valid", in: &EdgeCompleteness{}},
		{
			name:    "governed predicates without a mode is refused",
			in:      &EdgeCompleteness{GovernedPredicates: []string{"feeds"}},
			wantErr: "without a mode",
		},
		{
			name: "per_source with predicates is valid",
			in:   &EdgeCompleteness{Mode: EdgeCompletenessPerSource, GovernedPredicates: []string{"feeds", "hasPoint"}},
		},
		{
			name:    "per_source with no predicates is refused",
			in:      &EdgeCompleteness{Mode: EdgeCompletenessPerSource},
			wantErr: "at least one governed_predicate",
		},
		{
			name:    "per_source with a blank predicate is refused",
			in:      &EdgeCompleteness{Mode: EdgeCompletenessPerSource, GovernedPredicates: []string{"feeds", "  "}},
			wantErr: "empty predicate",
		},
		{
			name:    "unknown mode is refused",
			in:      &EdgeCompleteness{Mode: "per_predicate", GovernedPredicates: []string{"feeds"}},
			wantErr: "unsupported edge_completeness mode",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.in.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestEdgeCompleteness_Asserted(t *testing.T) {
	t.Parallel()
	var nilEC *EdgeCompleteness
	if nilEC.Asserted() {
		t.Fatal("nil assertion must not report Asserted")
	}
	if (&EdgeCompleteness{}).Asserted() {
		t.Fatal("partial mode must not report Asserted")
	}
	if !(&EdgeCompleteness{Mode: EdgeCompletenessPerSource}).Asserted() {
		t.Fatal("per_source must report Asserted")
	}
}

// decodeHeaderLine reads the artifact's first NDJSON line back into a Header, so
// a test asserts on what the writer actually emitted rather than on the struct it
// holds in memory.
func decodeHeaderLine(t *testing.T, body []byte) Header {
	t.Helper()
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		t.Fatal("artifact body is empty")
	}
	first := strings.SplitN(trimmed, "\n", 2)[0]
	var h Header
	if err := json.Unmarshal([]byte(first), &h); err != nil {
		t.Fatalf("decode header line %q: %v", first, err)
	}
	return h
}

func TestWithEdgeCompleteness_RidesTheHeader(t *testing.T) {
	t.Parallel()
	fc := &FakeCommitClient{}
	w := NewDataWriter(fc, "example-kit", WithEdgeCompleteness(EdgeCompleteness{
		Mode:               EdgeCompletenessPerSource,
		GovernedPredicates: []string{"feeds", "hasPoint"},
	}))
	if err := w.WriteVertex(context.Background(), Vertex{ID: "AHU-01", EntityType: "brick:AHU"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h := decodeHeaderLine(t, fc.LastBody())
	if h.EdgeCompleteness == nil {
		t.Fatal("header carries no edge_completeness")
	}
	if h.EdgeCompleteness.Mode != EdgeCompletenessPerSource {
		t.Fatalf("mode = %q, want %q", h.EdgeCompleteness.Mode, EdgeCompletenessPerSource)
	}
	if got := h.EdgeCompleteness.GovernedPredicates; len(got) != 2 || got[0] != "feeds" || got[1] != "hasPoint" {
		t.Fatalf("governed_predicates = %v", got)
	}
	// FormatVersion must NOT move for an additive field: a bump would reject
	// every artifact written by an existing kit.
	if h.FormatVersion != FormatVersion {
		t.Fatalf("format_version = %d, want %d", h.FormatVersion, FormatVersion)
	}
}

func TestWithEdgeCompleteness_AbsentByDefault(t *testing.T) {
	t.Parallel()
	fc := &FakeCommitClient{}
	w := NewDataWriter(fc, "example-kit")
	if err := w.WriteVertex(context.Background(), Vertex{ID: "AHU-01", EntityType: "brick:AHU"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	body := fc.LastBody()
	if h := decodeHeaderLine(t, body); h.EdgeCompleteness != nil {
		t.Fatalf("edge_completeness must be absent by default, got %+v", h.EdgeCompleteness)
	}
	// The serialized header must not carry the key at all, so a platform that
	// predates the field sees a byte-identical shape.
	if strings.Contains(string(body), "edge_completeness") {
		t.Fatal("header line must not mention edge_completeness when unset")
	}
}

func TestWithEdgeCompleteness_PartialModeLeavesHeaderClean(t *testing.T) {
	t.Parallel()
	fc := &FakeCommitClient{}
	w := NewDataWriter(fc, "example-kit", WithEdgeCompleteness(EdgeCompleteness{Mode: EdgeCompletenessPartial}))
	if err := w.WriteVertex(context.Background(), Vertex{ID: "A", EntityType: "T"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if h := decodeHeaderLine(t, fc.LastBody()); h.EdgeCompleteness != nil {
		t.Fatalf("partial mode must leave the header field absent, got %+v", h.EdgeCompleteness)
	}
}

func TestWithEdgeCompleteness_InvalidBlocksWritesAndClose(t *testing.T) {
	t.Parallel()
	fc := &FakeCommitClient{}
	w := NewDataWriter(fc, "example-kit", WithEdgeCompleteness(EdgeCompleteness{
		Mode: EdgeCompletenessPerSource, // no governed predicates
	}))
	if err := w.WriteVertex(context.Background(), Vertex{ID: "A", EntityType: "T"}); err == nil {
		t.Fatal("WriteVertex must surface the option error")
	}
	if err := w.WriteEdge(context.Background(), Edge{RelationshipType: "feeds", SourceID: "A", TargetID: "B"}); err == nil {
		t.Fatal("WriteEdge must surface the option error")
	}
	if _, err := w.Close(context.Background()); err == nil {
		t.Fatal("Close must refuse to commit")
	}
	if fc.CompleteCalls() != 0 {
		t.Fatal("a mis-configured writer must never commit an artifact")
	}
}

func TestWithEdgeCompleteness_RejectedOnAnOntologyWriter(t *testing.T) {
	t.Parallel()
	fc := &FakeCommitClient{}
	w := NewOntologyWriter(fc, "example-kit", WithEdgeCompleteness(EdgeCompleteness{
		Mode:               EdgeCompletenessPerSource,
		GovernedPredicates: []string{"feeds"},
	}))
	if _, err := w.Close(context.Background()); err == nil {
		t.Fatal("an ontology artifact carries no edges; the assertion must be refused")
	}
	if fc.CompleteCalls() != 0 {
		t.Fatal("must not commit")
	}
}

func TestWithEdgeCompleteness_CopiesTheCallerSlice(t *testing.T) {
	t.Parallel()
	preds := []string{"feeds"}
	fc := &FakeCommitClient{}
	w := NewDataWriter(fc, "example-kit", WithEdgeCompleteness(EdgeCompleteness{
		Mode:               EdgeCompletenessPerSource,
		GovernedPredicates: preds,
	}))
	preds[0] = "mutated"
	if err := w.WriteVertex(context.Background(), Vertex{ID: "A", EntityType: "T"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h := decodeHeaderLine(t, fc.LastBody())
	if h.EdgeCompleteness.GovernedPredicates[0] != "feeds" {
		t.Fatalf("the writer must not alias the caller's slice, got %v", h.EdgeCompleteness.GovernedPredicates)
	}
}

func TestWithEdgeCompleteness_NilOptionIsIgnored(t *testing.T) {
	t.Parallel()
	fc := &FakeCommitClient{}
	w := NewDataWriter(fc, "example-kit", nil)
	if err := w.WriteVertex(context.Background(), Vertex{ID: "A", EntityType: "T"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
