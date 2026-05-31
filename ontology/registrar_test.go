package ontology_test

import (
	"context"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/ontology"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

func TestRegistrar_RegisterTypes_StreamsAllEntries(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	reg := ontology.NewRegistrar(w)

	req := ontology.RegisterTypesRequest{
		EntityTypes: []ontology.EntityTypeDef{
			{Name: "alpha", Description: map[string]string{"en": "first"}},
			{Name: "beta", Description: map[string]string{"en": "second"}, ParentType: "alpha"},
			{Name: "gamma", Properties: []ontology.TypeProperty{
				{Name: "prop1", Type: "STRING"},
			}},
		},
		TypeHierarchy: []ontology.TypeHierarchyEntry{
			{TypeName: "beta", ParentType: "alpha"},
		},
	}

	if err := reg.RegisterTypes(context.Background(), req); err != nil {
		t.Fatalf("RegisterTypes: %v", err)
	}

	// Close to flush. Registrar deliberately leaves the writer open
	// so kit code can mix entry kinds; it's the SDK server's job to
	// close at request end.
	receipt, err := w.Close(context.Background())
	if err != nil {
		t.Fatalf("writer Close: %v", err)
	}

	// 3 entity types + 1 hierarchy entry = 4 records.
	if receipt.EntryCount != 4 {
		t.Errorf("EntryCount = %d, want 4 (3 types + 1 hierarchy)", receipt.EntryCount)
	}

	body := string(fc.LastBody())
	for _, want := range []string{"alpha", "beta", "gamma", `"kind":"entity_type"`, `"kind":"hierarchy"`} {
		if !contains(body, want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}
}

func TestRegistrar_RejectsEmpty(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	reg := ontology.NewRegistrar(w)

	if err := reg.RegisterTypes(context.Background(), ontology.RegisterTypesRequest{}); err == nil {
		t.Error("RegisterTypes with no entity types should return error")
	}
}

func TestRegistrar_NilWriter(t *testing.T) {
	t.Parallel()
	reg := ontology.NewRegistrar(nil)
	err := reg.RegisterTypes(context.Background(), ontology.RegisterTypesRequest{
		EntityTypes: []ontology.EntityTypeDef{{Name: "x"}},
	})
	if err == nil {
		t.Error("expected error from nil writer")
	}
}

func TestRegistrar_PassesPropertiesThrough(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	w := transfer.NewOntologyWriter(fc, "test-kit")
	reg := ontology.NewRegistrar(w)

	if err := reg.RegisterTypes(context.Background(), ontology.RegisterTypesRequest{
		EntityTypes: []ontology.EntityTypeDef{
			{Name: "with_props", Properties: []ontology.TypeProperty{
				{Name: "p1", Type: "STRING", Required: true},
				{Name: "p2", Type: "INTEGER"},
			}},
		},
	}); err != nil {
		t.Fatalf("RegisterTypes: %v", err)
	}
	_, _ = w.Close(context.Background())

	body := string(fc.LastBody())
	for _, want := range []string{`"name":"p1"`, `"required":true`, `"name":"p2"`, `"type":"INTEGER"`} {
		if !contains(body, want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
