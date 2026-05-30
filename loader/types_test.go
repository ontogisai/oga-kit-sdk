package loader_test

import (
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
