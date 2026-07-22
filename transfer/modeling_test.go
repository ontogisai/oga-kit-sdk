package transfer_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// TestEntityTypeDef_LogicalWireRoundTrip verifies a kind=ontology loader can
// emit an EntityTypeDef carrying Materialization=logical + PhysicalType, and
// the fields survive the NDJSON wire encoding the writer produces.
func TestEntityTypeDef_LogicalWireRoundTrip(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "oga-kit-sj24k")
	ctx := context.Background()

	if err := w.WriteEntityType(ctx, transfer.EntityTypeDef{
		Name:            "brick_AHU",
		ParentType:      "brick_Equipment",
		Category:        "equipment",
		Materialization: transfer.MaterializationLogical,
		PhysicalType:    "Equipment",
	}); err != nil {
		t.Fatalf("WriteEntityType: %v", err)
	}
	if _, err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := fc.LastBody()
	for _, want := range []string{
		`"kind":"entity_type"`,
		`"name":"brick_AHU"`,
		`"materialization":"logical"`,
		`"physical_type":"Equipment"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}

	// Decode the envelope value back into an EntityTypeDef and confirm the
	// hybrid fields round-trip.
	got := decodeEntityType(t, body)
	if got.Materialization != transfer.MaterializationLogical {
		t.Errorf("decoded Materialization = %q, want logical", got.Materialization)
	}
	if got.PhysicalType != "Equipment" {
		t.Errorf("decoded PhysicalType = %q, want Equipment", got.PhysicalType)
	}
}

// TestEntityTypeDef_PhysicalDefaultOmitsFields proves back-compat: an
// EntityTypeDef with no Materialization set does NOT emit the hybrid fields
// (omitempty), and decodes back with an empty Materialization that the
// platform reads as physical.
func TestEntityTypeDef_PhysicalDefaultOmitsFields(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "built-environment")
	ctx := context.Background()

	if err := w.WriteEntityType(ctx, transfer.EntityTypeDef{Name: "WorkOrder"}); err != nil {
		t.Fatalf("WriteEntityType: %v", err)
	}
	if _, err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := string(fc.LastBody())
	if strings.Contains(body, "materialization") {
		t.Errorf("body should omit materialization for a physical type\nbody: %s", body)
	}
	if strings.Contains(body, "physical_type") {
		t.Errorf("body should omit physical_type for a physical type\nbody: %s", body)
	}

	got := decodeEntityType(t, fc.LastBody())
	if got.Materialization != "" {
		t.Errorf("decoded Materialization = %q, want empty (⇒ physical)", got.Materialization)
	}
}

// TestRelationshipTypeDef_JSONContract locks the RelationshipTypeDef JSON
// contract (OGA-584 C7): the hybrid Materialization + PhysicalType fields
// round-trip with the expected json tags, and both are omitted when empty
// (a physical relationship type stays back-compatible on the wire).
//
// RelationshipTypeDef is the shared ontology relationship contract; the
// platform registers relationship types from manifest YAML and resolves new
// predicates onto RELATES at edge-write time, so the type is not streamed
// through the transfer writer (there is no WriteRelationshipType). This test
// therefore exercises the JSON shape directly rather than a writer path.
func TestRelationshipTypeDef_JSONContract(t *testing.T) {
	t.Parallel()

	// Logical: hybrid fields present and correctly tagged.
	logical := transfer.RelationshipTypeDef{
		Name:            "feeds",
		SourceType:      "brick_AHU",
		TargetType:      "brick_VAV",
		Cardinality:     "one_to_many",
		Materialization: transfer.MaterializationLogical,
		PhysicalType:    "RELATES",
	}
	raw, err := json.Marshal(logical)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"name":"feeds"`,
		`"source_type":"brick_AHU"`,
		`"target_type":"brick_VAV"`,
		`"cardinality":"one_to_many"`,
		`"materialization":"logical"`,
		`"physical_type":"RELATES"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("marshaled JSON missing %q\njson: %s", want, raw)
		}
	}
	var back transfer.RelationshipTypeDef
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, logical) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", back, logical)
	}

	// Physical (default): hybrid fields omitted, back-compatible on the wire.
	physical := transfer.RelationshipTypeDef{Name: "equipmentHasSchedule"}
	raw, err = json.Marshal(physical)
	if err != nil {
		t.Fatalf("marshal physical: %v", err)
	}
	if strings.Contains(string(raw), "materialization") {
		t.Errorf("physical relationship type should omit materialization\njson: %s", raw)
	}
	if strings.Contains(string(raw), "physical_type") {
		t.Errorf("physical relationship type should omit physical_type\njson: %s", raw)
	}
}

// envelope is the wire wrapper the writer emits per non-header record.
type envelope struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value"`
}

// decodeEntityType finds the first entity_type envelope in an NDJSON body and
// decodes its value.
func decodeEntityType(t *testing.T, body []byte) transfer.EntityTypeDef {
	t.Helper()
	raw := findEnvelopeValue(t, body, string(transfer.EntryEntityType))
	var et transfer.EntityTypeDef
	if err := json.Unmarshal(raw, &et); err != nil {
		t.Fatalf("decode EntityTypeDef: %v", err)
	}
	return et
}

func findEnvelopeValue(t *testing.T, body []byte, kind string) json.RawMessage {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue // header line or other shape
		}
		if env.Kind == kind {
			return env.Value
		}
	}
	t.Fatalf("no %q envelope found in body: %s", kind, body)
	return nil
}
