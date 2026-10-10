package transfer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVertex_Lifecycle_OmittedWhenEmpty verifies the additive, back-compatible
// contract (OGA-916): a vertex that does not set Lifecycle marshals WITHOUT the
// `lifecycle` field, so every existing kit/loader is byte-for-byte unaffected.
//
// This is the property that makes the wire opt-in real. If the field were
// emitted as `"lifecycle":""` the platform would still read it as present, but
// every existing artifact's bytes (and therefore its content hash) would change.
func TestVertex_Lifecycle_OmittedWhenEmpty(t *testing.T) {
	v := Vertex{ID: "ahu-1", EntityType: "brick_Equipment"}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "lifecycle") {
		t.Errorf("an unset Lifecycle must be omitted, got: %s", b)
	}
}

// TestVertex_Lifecycle_RoundTrip pins the wire key and value a removal carries,
// because the platform matches on both.
func TestVertex_Lifecycle_RoundTrip(t *testing.T) {
	v := Vertex{
		ID:         "ahu-1",
		EntityType: "brick_Equipment",
		Lifecycle:  LifecycleRemoved,
		// A kit's removal reason rides as an ordinary property — the platform
		// persists it onto the tombstone without interpreting it, so no wire
		// field is needed for it.
		Properties: map[string]any{"removal_reason": "kit-supplied, platform-opaque"},
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"lifecycle":"removed"`) {
		t.Errorf("unexpected wire shape: %s", b)
	}

	var got Vertex
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Lifecycle != LifecycleRemoved {
		t.Errorf("Lifecycle lost in round-trip: %q", got.Lifecycle)
	}
	if got.Properties["removal_reason"] != "kit-supplied, platform-opaque" {
		t.Errorf("a kit-carried reason must survive as an ordinary property: %+v", got.Properties)
	}
}

// TestLifecycle_RecognizedValues locks the two recognised tokens. They are part
// of the kit↔platform contract, so a rename here is a breaking change and must
// fail loudly rather than silently ceasing to match on the platform side.
func TestLifecycle_RecognizedValues(t *testing.T) {
	if LifecyclePresent != "present" {
		t.Errorf("LifecyclePresent = %q, want \"present\"", LifecyclePresent)
	}
	if LifecycleRemoved != "removed" {
		t.Errorf("LifecycleRemoved = %q, want \"removed\"", LifecycleRemoved)
	}
}

// TestVertex_Lifecycle_WriterAcceptsAnyValue documents that the SDK does NOT
// validate Lifecycle, matching RelationshipTypeDef.Cardinality ("free-form to
// the SDK; the platform validates against its recognized set").
//
// Two reasons this is deliberate rather than an omission:
//   - The platform is the only layer that can fail closed meaningfully, and it
//     does: an unrecognised value ingests as ordinary content and tombstones
//     nothing. Rejecting here would merely move the error earlier for a kit that
//     is, in the platform's eyes, simply not asserting a removal.
//   - A kit must not be blocked from emitting a future third value (a
//     `superseded` assertion) by the SDK version it happens to be built against.
func TestVertex_Lifecycle_WriterAcceptsAnyValue(t *testing.T) {
	w := &NopWriter{}
	for _, lc := range []Lifecycle{"", LifecyclePresent, LifecycleRemoved, "a-value-the-sdk-has-never-heard-of"} {
		if err := w.WriteVertex(t.Context(), Vertex{EntityType: "brick_Equipment", Lifecycle: lc}); err != nil {
			t.Errorf("WriteVertex(Lifecycle=%q) = %v, want nil", lc, err)
		}
	}
}
