package transfer_test

import (
	"context"
	"encoding/json"
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

// TestRelationshipTypeDef_WireRoundTrip verifies WriteRelationshipType emits a
// relationship_type envelope carrying the endpoint fields plus the hybrid
// Materialization + PhysicalType (OGA-584 C7).
func TestRelationshipTypeDef_WireRoundTrip(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "oga-kit-sj24k")
	ctx := context.Background()

	if err := w.WriteRelationshipType(ctx, transfer.RelationshipTypeDef{
		Name:            "feeds",
		SourceType:      "brick_AHU",
		TargetType:      "brick_VAV",
		Cardinality:     "one_to_many",
		Materialization: transfer.MaterializationLogical,
		PhysicalType:    "RELATES",
	}); err != nil {
		t.Fatalf("WriteRelationshipType: %v", err)
	}
	if _, err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := fc.LastBody()
	for _, want := range []string{
		`"kind":"relationship_type"`,
		`"name":"feeds"`,
		`"source_type":"brick_AHU"`,
		`"target_type":"brick_VAV"`,
		`"cardinality":"one_to_many"`,
		`"materialization":"logical"`,
		`"physical_type":"RELATES"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}

	got := decodeRelationshipType(t, body)
	if got.Materialization != transfer.MaterializationLogical || got.PhysicalType != "RELATES" {
		t.Errorf("decoded relationship hybrid fields = (%q,%q), want (logical,RELATES)",
			got.Materialization, got.PhysicalType)
	}
	if got.SourceType != "brick_AHU" || got.TargetType != "brick_VAV" {
		t.Errorf("decoded endpoints = (%q,%q)", got.SourceType, got.TargetType)
	}
}

// TestWriteRelationshipType_RequiresName confirms the writer rejects a
// relationship type with no name.
func TestWriteRelationshipType_RequiresName(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "k")
	if err := w.WriteRelationshipType(context.Background(), transfer.RelationshipTypeDef{}); err == nil {
		t.Error("expected error for relationship type with no name")
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

// decodeRelationshipType finds the first relationship_type envelope in an
// NDJSON body and decodes its value.
func decodeRelationshipType(t *testing.T, body []byte) transfer.RelationshipTypeDef {
	t.Helper()
	raw := findEnvelopeValue(t, body, string(transfer.EntryRelationshipType))
	var rt transfer.RelationshipTypeDef
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("decode RelationshipTypeDef: %v", err)
	}
	return rt
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
