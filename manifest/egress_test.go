package manifest

import (
	"strings"
	"testing"
)

// egressManifestYAML is the block a kit author writes for an egress component.
// Before OGA-810 this exact input was REJECTED at parse time — manifest.Parse
// sets KnownFields(true), so an unknown spec.egress_syncs was a hard error and an
// author could not lint an egress manifest locally at all.
const egressManifestYAML = `api_version: ontogis.ai/v1
kind: DomainKitManifest
metadata:
  name: sj24k
  version: 1.0.0
  display_name:
    en-US: SJ 24K
  description:
    en-US: 24K Core integration
spec:
  platform_version: ">=1.0.0"
  egress_syncs:
    - name: 24k-core-egress
      external_system: 24k-core
      entity_types:
        - name: rec_Site
        - name: rec_Building
          parent_edge: hasPart
        - name: brick_Equipment
      credential_refs:
        - 24k-core-api-key
      batch_size: 100
      max_in_flight: 4
      container:
        image: ghcr.io/ontogisai/oga-kit-sj24k/24k-core-egress@sha256:abc123
        port: 8600
        env:
          CORE_BASE_URL: "secret://24k-core-base-url"
`

func TestParse_EgressSyncsBlock(t *testing.T) {
	m, err := Parse(strings.NewReader(egressManifestYAML))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	if len(m.Spec.EgressSyncs) != 1 {
		t.Fatalf("egress_syncs = %d, want 1", len(m.Spec.EgressSyncs))
	}
	e := &m.Spec.EgressSyncs[0]
	if e.Name != "24k-core-egress" {
		t.Errorf("name = %q", e.Name)
	}
	if e.ExternalSystem != "24k-core" {
		t.Errorf("external_system = %q", e.ExternalSystem)
	}
	// Declaration ORDER is load-bearing: the platform pushes a type to
	// completion before the next begins so a later type may reference an earlier
	// one, so the parsed slice must preserve it.
	want := []string{"rec_Site", "rec_Building", "brick_Equipment"}
	got := e.EntityTypeNames()
	if len(got) != len(want) {
		t.Fatalf("entity types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entity types = %v, want %v (order is load-bearing)", got, want)
		}
	}
	if e.EntityTypes[1].ParentEdge != "hasPart" {
		t.Errorf("entity_types[1].parent_edge = %q, want hasPart", e.EntityTypes[1].ParentEdge)
	}
	if len(e.CredentialRefs) != 1 || e.CredentialRefs[0] != "24k-core-api-key" {
		t.Errorf("credential_refs = %v", e.CredentialRefs)
	}
	if e.EffectiveBatchSize() != 100 {
		t.Errorf("batch size = %d, want the declared 100", e.EffectiveBatchSize())
	}
	if e.Container.Port != 8600 {
		t.Errorf("container.port = %d", e.Container.Port)
	}
	if e.Container.Env["CORE_BASE_URL"] != "secret://24k-core-base-url" {
		t.Errorf("container.env = %v", e.Container.Env)
	}
}

func TestEgress_EffectiveImagePrecedence(t *testing.T) {
	// container.image is canonical (OGA-637).
	both := EgressSyncSpec{
		Image:     "ghcr.io/x/legacy@sha256:aaa",
		Container: SidecarContainerSpec{Image: "ghcr.io/x/canonical@sha256:bbb"},
	}
	if got := both.EffectiveImage(); got != "ghcr.io/x/canonical@sha256:bbb" {
		t.Errorf("EffectiveImage = %q, want container.image to win", got)
	}
	// Top-level image remains the back-compat fallback.
	legacy := EgressSyncSpec{Image: "ghcr.io/x/legacy@sha256:aaa"}
	if got := legacy.EffectiveImage(); got != "ghcr.io/x/legacy@sha256:aaa" {
		t.Errorf("EffectiveImage = %q, want the top-level fallback", got)
	}
	if (&EgressSyncSpec{}).EffectiveImage() != "" {
		t.Error("EffectiveImage on an empty spec should be empty")
	}
}

func TestEgress_EffectiveBatchSizeDefaults(t *testing.T) {
	if got := (&EgressSyncSpec{}).EffectiveBatchSize(); got != DefaultEgressBatchSize {
		t.Errorf("EffectiveBatchSize = %d, want %d", got, DefaultEgressBatchSize)
	}
	if got := (&EgressSyncSpec{BatchSize: -5}).EffectiveBatchSize(); got != DefaultEgressBatchSize {
		t.Errorf("negative batch size = %d, want the default", got)
	}
}

// A hierarchical type is pushed sequentially whatever the kit declared:
// concurrent batches within it would let a child be pushed before its parent,
// which is exactly the ordering the level-by-level walk exists to guarantee.
func TestEgress_EffectiveMaxInFlightClampedForHierarchy(t *testing.T) {
	e := &EgressSyncSpec{MaxInFlight: 8}
	if got := e.EffectiveMaxInFlight(&EgressEntityTypeSpec{Name: "rec_Building", ParentEdge: "hasPart"}); got != 1 {
		t.Errorf("hierarchical max_in_flight = %d, want 1", got)
	}
	if got := e.EffectiveMaxInFlight(&EgressEntityTypeSpec{Name: "brick_Equipment"}); got != 8 {
		t.Errorf("flat max_in_flight = %d, want the declared 8", got)
	}
	if got := e.EffectiveMaxInFlight(nil); got != 8 {
		t.Errorf("nil entity type max_in_flight = %d, want 8", got)
	}
	if got := (&EgressSyncSpec{}).EffectiveMaxInFlight(nil); got != 1 {
		t.Errorf("undeclared max_in_flight = %d, want 1 (sequential)", got)
	}
	if got := (&EgressSyncSpec{MaxInFlight: -3}).EffectiveMaxInFlight(nil); got != 1 {
		t.Errorf("negative max_in_flight = %d, want 1", got)
	}
}

func TestEgress_EntityTypeNamesSkipsEmpty(t *testing.T) {
	e := &EgressSyncSpec{EntityTypes: []EgressEntityTypeSpec{{Name: "A"}, {Name: ""}, {Name: "B"}}}
	got := e.EntityTypeNames()
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("EntityTypeNames = %v", got)
	}
	if (&EgressSyncSpec{}).EntityTypeNames() != nil {
		t.Error("EntityTypeNames on an empty spec should be nil")
	}
}

// The validator mirrors the platform's domainkit.validateEgressSpecStructure, so
// an author sees the same rejection locally that the installer raises as
// OGA-EGRS-VAL-1002.
func TestValidateEgressSyncs(t *testing.T) {
	pinned := SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"}
	valid := func() EgressSyncSpec {
		return EgressSyncSpec{
			Name:           "e1",
			ExternalSystem: "24k-core",
			EntityTypes:    []EgressEntityTypeSpec{{Name: "Equipment"}},
			Container:      pinned,
		}
	}

	if err := validateEgressSyncs(nil); err != nil {
		t.Errorf("no egress components should validate: %v", err)
	}
	if err := validateEgressSyncs([]EgressSyncSpec{valid()}); err != nil {
		t.Errorf("valid component rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*EgressSyncSpec)
		wantSub string
	}{
		{"missing name", func(e *EgressSyncSpec) { e.Name = "" }, "name is required"},
		{"blank name", func(e *EgressSyncSpec) { e.Name = "   " }, "name is required"},
		{
			// Never defaultable: it is the value recorded as each pushed entity's
			// system of record, and a wrong one makes the correlation unresolvable.
			"missing external_system",
			func(e *EgressSyncSpec) { e.ExternalSystem = "" },
			"external_system is required",
		},
		{"blank external_system", func(e *EgressSyncSpec) { e.ExternalSystem = "  " }, "external_system is required"},
		{"no entity types", func(e *EgressSyncSpec) { e.EntityTypes = nil }, "at least one entity type"},
		{
			"entity types all unnamed",
			func(e *EgressSyncSpec) { e.EntityTypes = []EgressEntityTypeSpec{{}, {}} },
			"at least one entity type",
		},
		{"missing image", func(e *EgressSyncSpec) { e.Container.Image = "" }, "image is required"},
		{
			// A mutable tag means the running component is not the one that was
			// signature-verified at upload.
			"unpinned image",
			func(e *EgressSyncSpec) { e.Container.Image = "ghcr.io/x/e:v1.0.0" },
			"digest-pinned",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := valid()
			tc.mutate(&e)
			err := validateEgressSyncs([]EgressSyncSpec{e})
			if err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantSub)
			}
			if !strings.Contains(err.Error(), "spec.egress_syncs[0]") {
				t.Errorf("error = %q, want it to locate the offending entry", err)
			}
		})
	}

	t.Run("duplicate name", func(t *testing.T) {
		a, b := valid(), valid()
		err := validateEgressSyncs([]EgressSyncSpec{a, b})
		if err == nil || !strings.Contains(err.Error(), "duplicates") {
			t.Fatalf("duplicate component name accepted: %v", err)
		}
	})

	// The top-level image fallback satisfies the image check.
	t.Run("legacy top-level image", func(t *testing.T) {
		e := valid()
		e.Container.Image = ""
		e.Image = "ghcr.io/x/e@sha256:abc"
		if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
			t.Errorf("legacy image rejected: %v", err)
		}
	})
}

// Validate() must reach the egress validator — wiring the struct without the
// call would let a malformed block parse cleanly and fail only at install.
func TestValidate_ReachesEgressValidator(t *testing.T) {
	bad := strings.Replace(egressManifestYAML,
		"      external_system: 24k-core\n", "", 1)
	m, err := Parse(strings.NewReader(bad))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = Validate(m)
	if err == nil {
		t.Fatal("manifest missing external_system validated")
	}
	if !strings.Contains(err.Error(), "external_system is required") {
		t.Errorf("error = %q", err)
	}
}

// The SDK deliberately does NOT check that an entity type exists in the tenant's
// active ontology (the platform's OGA-EGRS-VAL-1001). That needs per-tenant state
// no local lint can see, so a made-up type must parse and validate here — and be
// caught at install.
func TestValidateEgressSyncs_DoesNotCheckOntology(t *testing.T) {
	e := EgressSyncSpec{
		Name:           "e1",
		ExternalSystem: "24k-core",
		EntityTypes:    []EgressEntityTypeSpec{{Name: "NoSuchTypeAnywhere"}},
		Container:      SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Errorf("unknown entity type rejected locally: %v — the ontology check is install-time only", err)
	}
}
