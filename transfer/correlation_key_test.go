package transfer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVertex_CorrelationKey_OmittedWhenNil verifies the additive,
// back-compatible contract: a vertex that does not set CorrelationKey
// marshals without the `correlation_key` field, so existing kits/loaders
// that never use it are byte-for-byte unaffected.
func TestVertex_CorrelationKey_OmittedWhenNil(t *testing.T) {
	v := Vertex{ID: "wo-1", EntityType: "WorkOrder"}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "correlation_key") {
		t.Errorf("nil CorrelationKey must be omitted, got: %s", b)
	}
}

// TestVertex_CorrelationKey_RoundTrip verifies the merge-path shape:
// a vertex carrying a CorrelationKey with an empty ID round-trips with
// both external reference fields intact.
func TestVertex_CorrelationKey_RoundTrip(t *testing.T) {
	v := Vertex{
		EntityType:     "WorkOrder",
		Properties:     map[string]any{"status": "completed"},
		CorrelationKey: &CorrelationKey{ExternalSystem: "contract_wo_mgmt", ExternalRecordID: "WO-1234"},
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Vertex
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "" {
		t.Errorf("ID should be empty for an external-ref merge record, got %q", got.ID)
	}
	if got.CorrelationKey == nil {
		t.Fatal("CorrelationKey lost in round-trip")
	}
	if got.CorrelationKey.ExternalSystem != "contract_wo_mgmt" ||
		got.CorrelationKey.ExternalRecordID != "WO-1234" {
		t.Errorf("CorrelationKey mismatch: %+v", got.CorrelationKey)
	}
	// Wire keys must match the platform-side contract.
	if !strings.Contains(string(b), `"external_system":"contract_wo_mgmt"`) ||
		!strings.Contains(string(b), `"external_record_id":"WO-1234"`) {
		t.Errorf("unexpected wire shape: %s", b)
	}
}
