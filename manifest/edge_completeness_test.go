package manifest

import (
	"strings"
	"testing"

	"bytes"
)

// The strict parser must ACCEPT a connector's edge_completeness block.
//
// This is the whole reason the SDK mirrors the field (OGA-930). Parse sets
// KnownFields(true), so before the mirror existed a kit declaring the block the
// PLATFORM reads would fail its own local validation — the kit author would see a
// parse error on a manifest that installs fine, which is the worst possible split.
func TestParse_AcceptsConnectorEdgeCompleteness(t *testing.T) {
	t.Parallel()
	m, err := Parse(bytes.NewReader([]byte(`
api_version: v1
kind: DomainKit
metadata:
  name: test-kit
  version: 1.0.0
spec:
  platform_version: ">=0.1.0"
  source_connectors:
    - name: asset-sync
      bindings:
        - id: wo
          external_system: wo_mgmt
          source_type: wo_status_feed
      container:
        image: ghcr.io/acme/asset-sync@sha256:abc
      edge_completeness:
        mode: per_source
        governed_predicates_file: predicates/forward.json
`)))
	if err != nil {
		t.Fatalf("strict parse must accept edge_completeness: %v", err)
	}
	if len(m.Spec.SourceConnectors) != 1 {
		t.Fatalf("connectors = %d, want 1", len(m.Spec.SourceConnectors))
	}
	ec := m.Spec.SourceConnectors[0].EdgeCompleteness
	if ec == nil {
		t.Fatal("edge_completeness was dropped")
	}
	if ec.Mode != "per_source" {
		t.Errorf("mode = %q, want per_source", ec.Mode)
	}
	if ec.GovernedPredicatesFile != "predicates/forward.json" {
		t.Errorf("governed_predicates_file = %q", ec.GovernedPredicatesFile)
	}
}

// Absent is the default and asserts nothing — the pre-feature behavior.
func TestParse_ConnectorWithoutEdgeCompleteness(t *testing.T) {
	t.Parallel()
	m, err := Parse(bytes.NewReader([]byte(`
api_version: v1
kind: DomainKit
metadata:
  name: test-kit
  version: 1.0.0
spec:
  platform_version: ">=0.1.0"
  source_connectors:
    - name: asset-sync
      bindings:
        - id: wo
          external_system: wo_mgmt
          source_type: wo_status_feed
      container:
        image: ghcr.io/acme/asset-sync@sha256:abc
`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Spec.SourceConnectors[0].EdgeCompleteness != nil {
		t.Error("edge_completeness must be nil when undeclared, so nothing is asserted")
	}
}

// An unknown key INSIDE the block is still refused, so a typo in `mode` or
// `governed_predicates_file` is a loud error rather than a silently inert feed.
//
// Worth asserting explicitly: a nested struct reached through a pointer does not
// always inherit the parent decoder's KnownFields setting, which has bitten this
// manifest before.
func TestParse_RejectsUnknownEdgeCompletenessKey(t *testing.T) {
	t.Parallel()
	_, err := Parse(bytes.NewReader([]byte(`
api_version: v1
kind: DomainKit
metadata:
  name: test-kit
  version: 1.0.0
spec:
  platform_version: ">=0.1.0"
  source_connectors:
    - name: asset-sync
      bindings:
        - id: wo
          external_system: wo_mgmt
          source_type: wo_status_feed
      container:
        image: ghcr.io/acme/asset-sync@sha256:abc
      edge_completeness:
        mode: per_source
        governed_predicate_file: predicates/forward.json
`)))
	if err == nil {
		t.Fatal("a misspelled key inside edge_completeness must be refused")
	}
	if !strings.Contains(err.Error(), "governed_predicate_file") {
		t.Errorf("the error should name the offending key, got: %v", err)
	}
}
