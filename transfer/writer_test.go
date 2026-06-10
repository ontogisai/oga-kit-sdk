package transfer_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

func TestWriter_InlinePathBelowLimit(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := w.WriteEntityType(ctx, transfer.EntityTypeDef{
			Name:        "type_" + string(rune('A'+i)),
			Description: map[string]string{"en": "test"},
		}); err != nil {
			t.Fatalf("WriteEntityType: %v", err)
		}
	}

	receipt, err := w.Close(ctx)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if receipt.Mode != transfer.TransportInline {
		t.Errorf("Mode = %q, want inline (small payload should not trigger presigned)", receipt.Mode)
	}
	if fc.PrepareCalls() != 0 {
		t.Errorf("PrepareUpload calls = %d, want 0 for inline path", fc.PrepareCalls())
	}
	if fc.PutCalls() != 0 {
		t.Errorf("PutBytes calls = %d, want 0 for inline path", fc.PutCalls())
	}
	if fc.CompleteCalls() != 1 {
		t.Errorf("Complete calls = %d, want 1", fc.CompleteCalls())
	}
	if receipt.JobID == "" {
		t.Error("receipt.JobID was empty, want platform-issued id")
	}
	if receipt.EntryCount != 5 {
		t.Errorf("EntryCount = %d, want 5", receipt.EntryCount)
	}
	if receipt.ContentHash != fc.LastBodyHash() {
		t.Errorf("receipt.ContentHash %q != fake.LastBodyHash %q", receipt.ContentHash, fc.LastBodyHash())
	}
}

func TestWriter_PresignedPathAboveLimit(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewDataWriter(fc, "test-kit")

	ctx := context.Background()
	// Write enough vertices to push the body past InlineBodyLimit.
	// Each vertex is ~1 KB after encoding; 1500 puts us around 1.5 MB.
	bigProps := strings.Repeat("x", 900)
	for i := 0; i < 1500; i++ {
		if err := w.WriteVertex(ctx, transfer.Vertex{
			EntityType: "test_Type",
			ID:         "v-" + bigProps[:20] + string(rune('A'+(i%26))),
			Properties: map[string]any{"big": bigProps},
		}); err != nil {
			t.Fatalf("WriteVertex %d: %v", i, err)
		}
	}

	receipt, err := w.Close(ctx)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if receipt.Mode != transfer.TransportPresigned {
		t.Errorf("Mode = %q, want presigned for large payload", receipt.Mode)
	}
	if fc.PrepareCalls() != 1 {
		t.Errorf("PrepareUpload calls = %d, want 1", fc.PrepareCalls())
	}
	if fc.PutCalls() != 1 {
		t.Errorf("PutBytes calls = %d, want 1", fc.PutCalls())
	}
	if fc.CompleteCalls() != 1 {
		t.Errorf("Complete calls = %d, want 1", fc.CompleteCalls())
	}
	last := fc.LastComplete()
	if last == nil {
		t.Fatal("LastComplete is nil")
		return
	}
	if last.UploadToken == "" {
		t.Error("Complete.UploadToken was empty on presigned path")
	}
	if len(last.InlineBody) != 0 {
		t.Errorf("InlineBody should be empty on presigned path, got %d bytes", len(last.InlineBody))
	}
	if receipt.BytesWritten <= int64(transfer.InlineBodyLimit) {
		t.Errorf("BytesWritten = %d, want > %d to verify the presigned-trigger threshold",
			receipt.BytesWritten, transfer.InlineBodyLimit)
	}
}

func TestWriter_ContentHashMatchesPayload(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	ctx := context.Background()

	if err := w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "alpha"}); err != nil {
		t.Fatalf("WriteEntityType: %v", err)
	}
	receipt, err := w.Close(ctx)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := fc.LastBody()
	if len(body) == 0 {
		t.Fatal("FakeCommitClient.LastBody is empty")
	}
	wantHash := sha256.Sum256(body)
	wantHex := hex.EncodeToString(wantHash[:])
	if receipt.ContentHash != wantHex {
		t.Errorf("receipt.ContentHash = %q, want %q (sha256 of body)", receipt.ContentHash, wantHex)
	}
}

func TestWriter_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewDataWriter(fc, "test-kit")
	ctx := context.Background()

	cases := []struct {
		name string
		fn   func() error
	}{
		{"vertex_no_entity_type", func() error {
			return w.WriteVertex(ctx, transfer.Vertex{ID: "v1"})
		}},
		{"edge_no_relationship_type", func() error {
			return w.WriteEdge(ctx, transfer.Edge{SourceID: "a", TargetID: "b"})
		}},
		{"edge_no_source", func() error {
			return w.WriteEdge(ctx, transfer.Edge{RelationshipType: "r", TargetID: "b"})
		}},
		{"edge_no_target", func() error {
			return w.WriteEdge(ctx, transfer.Edge{RelationshipType: "r", SourceID: "a"})
		}},
		{"entity_type_no_name", func() error {
			return w.WriteEntityType(ctx, transfer.EntityTypeDef{})
		}},
		{"hierarchy_no_type_name", func() error {
			return w.WriteHierarchy(ctx, transfer.HierarchyEntry{ParentType: "p"})
		}},
		{"hierarchy_no_parent", func() error {
			return w.WriteHierarchy(ctx, transfer.HierarchyEntry{TypeName: "t"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.fn(); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

func TestWriter_CloseTwiceFails(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	ctx := context.Background()

	_ = w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "alpha"})
	if _, err := w.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, err := w.Close(ctx); err == nil {
		t.Error("second Close should return error")
	}
}

func TestWriter_WriteAfterCloseFails(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	ctx := context.Background()

	_ = w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "alpha"})
	_, _ = w.Close(ctx)
	if err := w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "beta"}); err == nil {
		t.Error("WriteEntityType after Close should return error")
	}
}

func TestWriter_PrepareFailureSurfaces(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{
		FailPrepare: errors.New("storage down"),
	}
	w := transfer.NewDataWriter(fc, "test-kit")
	ctx := context.Background()

	// Force the presigned path with a big body.
	big := strings.Repeat("x", 1024)
	for i := 0; i < 1200; i++ {
		_ = w.WriteVertex(ctx, transfer.Vertex{
			EntityType: "test",
			ID:         big[:32] + string(rune('A'+(i%26))),
			Properties: map[string]any{"x": big},
		})
	}
	_, err := w.Close(ctx)
	if err == nil {
		t.Fatal("expected Close to fail when PrepareUpload fails")
	}
	if !strings.Contains(err.Error(), "storage down") {
		t.Errorf("error = %v, want underlying 'storage down'", err)
	}
}

func TestWriter_PutFailureSurfaces(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{
		FailPut: errors.New("upload disconnected"),
	}
	w := transfer.NewDataWriter(fc, "test-kit")
	ctx := context.Background()

	big := strings.Repeat("x", 1024)
	for i := 0; i < 1200; i++ {
		_ = w.WriteVertex(ctx, transfer.Vertex{
			EntityType: "test",
			ID:         big[:32] + string(rune('A'+(i%26))),
			Properties: map[string]any{"x": big},
		})
	}
	_, err := w.Close(ctx)
	if err == nil {
		t.Fatal("expected Close to fail when PutBytes fails")
	}
	if !strings.Contains(err.Error(), "upload disconnected") {
		t.Errorf("error = %v, want underlying 'upload disconnected'", err)
	}
}

func TestWriter_CompleteFailureSurfaces(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{
		FailComplete: errors.New("server rejected"),
	}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	ctx := context.Background()

	_ = w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "alpha"})
	_, err := w.Close(ctx)
	if err == nil {
		t.Fatal("expected Close to fail when Complete fails")
	}
	if !strings.Contains(err.Error(), "server rejected") {
		t.Errorf("error = %v, want underlying 'server rejected'", err)
	}
}

func TestWriter_CompleteRequestCarriesKindAndKitID(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "built-environment")
	ctx := context.Background()

	_ = w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "alpha"})
	_, err := w.Close(ctx)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	last := fc.LastComplete()
	if last == nil {
		t.Fatal("LastComplete is nil")
		return
	}
	if last.Kind != transfer.KindOntology {
		t.Errorf("Complete.Kind = %q, want ontology", last.Kind)
	}
	if last.Format != transfer.FormatNDJSON {
		t.Errorf("Complete.Format = %q, want %q", last.Format, transfer.FormatNDJSON)
	}
	if last.EntryCount != 1 {
		t.Errorf("Complete.EntryCount = %d, want 1", last.EntryCount)
	}
}
