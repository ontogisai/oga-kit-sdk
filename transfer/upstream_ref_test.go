package transfer

import (
	"context"
	"strings"
	"testing"
)

// The handle must reach loader.complete on the INLINE commit path.
func TestWithUpstreamRef_CarriedOnInlineCommit(t *testing.T) {
	t.Parallel()
	fake := &FakeCommitClient{}
	w := NewDataWriter(fake, "kit-x", WithUpstreamRef("2026-09-07T10:15:00Z"))

	if err := w.WriteVertex(context.Background(), Vertex{ID: "v1", EntityType: "T"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := fake.LastComplete()
	if got == nil {
		t.Fatal("no CompleteRequest captured")
	}
	if got.UpstreamRef != "2026-09-07T10:15:00Z" {
		t.Errorf("UpstreamRef = %q, want the value set at construction", got.UpstreamRef)
	}
	if len(got.InlineBody) == 0 {
		t.Error("expected the inline path for a small artifact")
	}
}

// Absent stays absent. The platform must never receive a synthesised value,
// because a fabricated correlation handle points at the wrong upstream version.
func TestWithUpstreamRef_AbsentStaysEmpty(t *testing.T) {
	t.Parallel()
	fake := &FakeCommitClient{}
	w := NewDataWriter(fake, "kit-x")

	if err := w.WriteVertex(context.Background(), Vertex{ID: "v1", EntityType: "T"}); err != nil {
		t.Fatalf("WriteVertex: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := fake.LastComplete(); got.UpstreamRef != "" {
		t.Errorf("UpstreamRef = %q, want empty when unset", got.UpstreamRef)
	}
}

// Over-length fails the WRITER, before anything is uploaded.
//
// The platform rejects rather than truncates, so failing at construction turns
// a post-upload rejection into an immediate, local error — and nothing is
// committed. Asserted via the optErr path: every write and Close return it.
func TestWithUpstreamRef_OverLengthFailsBeforeCommit(t *testing.T) {
	t.Parallel()
	fake := &FakeCommitClient{}
	tooLong := strings.Repeat("x", UpstreamRefMaxBytes+1)
	w := NewDataWriter(fake, "kit-x", WithUpstreamRef(tooLong))

	err := w.WriteVertex(context.Background(), Vertex{ID: "v1", EntityType: "T"})
	if err == nil {
		t.Fatal("WriteVertex succeeded with an over-length upstream_ref")
	}
	if !strings.Contains(err.Error(), "upstream_ref") {
		t.Errorf("error should name the field, got: %v", err)
	}

	if _, closeErr := w.Close(context.Background()); closeErr == nil {
		t.Error("Close succeeded with an over-length upstream_ref")
	}
	if fake.LastComplete() != nil {
		t.Error("an artifact was committed despite the option failure")
	}
}

// Exactly at the bound is accepted — the limit is inclusive.
func TestWithUpstreamRef_AtBoundIsAccepted(t *testing.T) {
	t.Parallel()
	fake := &FakeCommitClient{}
	atLimit := strings.Repeat("y", UpstreamRefMaxBytes)
	w := NewDataWriter(fake, "kit-x", WithUpstreamRef(atLimit))

	if err := w.WriteVertex(context.Background(), Vertex{ID: "v1", EntityType: "T"}); err != nil {
		t.Fatalf("WriteVertex at the bound: %v", err)
	}
	if _, err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close at the bound: %v", err)
	}
	if got := fake.LastComplete().UpstreamRef; len(got) != UpstreamRefMaxBytes {
		t.Errorf("len(UpstreamRef) = %d, want %d", len(got), UpstreamRefMaxBytes)
	}
}

// The bound is measured in BYTES, not runes, because that is what the platform
// enforces. A multi-byte string under the rune count but over the byte count
// must be refused.
func TestWithUpstreamRef_BoundIsBytesNotRunes(t *testing.T) {
	t.Parallel()
	fake := &FakeCommitClient{}
	// "é" is 2 bytes; half the byte limit plus one rune exceeds the byte bound
	// while the rune count stays well under it.
	multi := strings.Repeat("é", (UpstreamRefMaxBytes/2)+1)
	if len([]rune(multi)) > UpstreamRefMaxBytes {
		t.Fatalf("fixture is not rune-under-bound: %d runes", len([]rune(multi)))
	}

	w := NewDataWriter(fake, "kit-x", WithUpstreamRef(multi))
	if err := w.WriteVertex(context.Background(), Vertex{ID: "v1", EntityType: "T"}); err == nil {
		t.Error("a byte-over-bound value was accepted; the bound must be bytes, not runes")
	}
}
