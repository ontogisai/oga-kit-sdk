package manifest

import (
	"strings"
	"testing"
)

// A connector opting in to outcome reports parses and validates.
//
// The flag is OPTIONAL and additive: it gates only whether the platform
// delivers a report, so it must not interact with any other validation rule.
func TestParse_SourceConnectorReceivesOutcomeReport(t *testing.T) {
	t.Parallel()
	const y = `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  source_connectors:
    - name: a-data-sync
      container:
        image: ghcr.io/example/a-data-sync@sha256:abc
        port: 8500
      receives_outcome_report: true
      bindings:
        - id: feed
          external_system: an_external_system
          source_type: a_feed
          modes: ["webhook"]
`
	m, err := Parse(strings.NewReader(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	conns := m.Spec.SourceConnectors
	if len(conns) != 1 {
		t.Fatalf("connectors = %d, want 1", len(conns))
	}
	if !conns[0].ReceivesOutcomeReport {
		t.Error("receives_outcome_report did not parse as true")
	}
}

// Absent means false, and the manifest stays valid — the pre-feature shape.
func TestParse_SourceConnectorOutcomeReportDefaultsFalse(t *testing.T) {
	t.Parallel()
	const y = `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  source_connectors:
    - name: a-data-sync
      container:
        image: ghcr.io/example/a-data-sync@sha256:abc
        port: 8500
      bindings:
        - id: feed
          external_system: an_external_system
          source_type: a_feed
          modes: ["webhook"]
`
	m, err := Parse(strings.NewReader(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Spec.SourceConnectors[0].ReceivesOutcomeReport {
		t.Error("receives_outcome_report defaulted to true; it must be opt-in")
	}
}

// The same flag on an egress component. Receipt is per SIDECAR, so both roles
// declare it independently and neither implies the other.
func TestParse_EgressSyncReceivesOutcomeReport(t *testing.T) {
	t.Parallel()
	const y = `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  egress_syncs:
    - name: a-egress-sync
      external_system: an_external_system
      receives_outcome_report: true
      container:
        image: ghcr.io/example/a-egress-sync@sha256:abc
        port: 8600
      entities_sync:
        - name: SomeType
`
	m, err := Parse(strings.NewReader(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(m.Spec.EgressSyncs) != 1 {
		t.Fatalf("egress components = %d, want 1", len(m.Spec.EgressSyncs))
	}
	if !m.Spec.EgressSyncs[0].ReceivesOutcomeReport {
		t.Error("receives_outcome_report did not parse as true on the egress component")
	}
}

// An unknown key is still refused. The strict decoder is what gives a kit
// author a local error instead of a silently ignored block, and adding a field
// must not loosen it.
func TestParse_StrictDecoderStillRejectsATypo(t *testing.T) {
	t.Parallel()
	const y = `
api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: kit
  version: 1.0.0
spec:
  platform_version: ">=1.0.0"
  source_connectors:
    - name: a-data-sync
      receives_outcome_reports: true
      container:
        image: ghcr.io/example/a-data-sync@sha256:abc
        port: 8500
      bindings:
        - id: feed
          external_system: an_external_system
          source_type: a_feed
          modes: ["webhook"]
`
	_, err := Parse(strings.NewReader(y))
	if err == nil {
		t.Fatal("a misspelled receives_outcome_reports was accepted; the decoder must stay strict")
	}
	// Assert it failed for THE TYPO, not for some unrelated defect in the
	// fixture. Without this the test passes vacuously the moment the base
	// manifest stops parsing for any other reason.
	if !strings.Contains(err.Error(), "receives_outcome_reports") {
		t.Errorf("error should name the misspelled field, got: %v", err)
	}
}
