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
          include_descendants: true
        - name: rec_Building
          parent_edges: [hasPart]
          hierarchical: true
          include_descendants: true
        - name: brick_Equipment
          parent_edges: [hasPart]
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
	if got := e.EntityTypes[1].ParentEdges; len(got) != 1 || got[0] != "hasPart" {
		t.Errorf("entity_types[1].parent_edges = %v, want [hasPart]", got)
	}
	if !e.EntityTypes[1].Hierarchical {
		t.Error("entity_types[1].hierarchical = false, want true")
	}
	if !e.EntityTypes[1].IncludeDescendants {
		t.Error("entity_types[1].include_descendants = false, want true")
	}
	// A cross-type reference declares an edge WITHOUT hierarchical: it needs the
	// owner resolved but no level walk.
	if e.EntityTypes[2].Hierarchical {
		t.Error("entity_types[2].hierarchical = true; a cross-type edge is not a hierarchy")
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
func TestEgress_EffectiveMaxInFlight(t *testing.T) {
	// No clamp and no entity-type argument. An earlier shape forced 1 for a
	// hierarchical type; concurrency is unsafe only ACROSS levels, and confining
	// batches to a level is the platform's scheduling job, not something a single
	// number can express.
	e := &EgressSyncSpec{MaxInFlight: 8}
	if got := e.EffectiveMaxInFlight(); got != 8 {
		t.Errorf("EffectiveMaxInFlight = %d, want the declared 8", got)
	}
	if got := (&EgressSyncSpec{}).EffectiveMaxInFlight(); got != 1 {
		t.Errorf("undeclared EffectiveMaxInFlight = %d, want 1", got)
	}
	if got := (&EgressSyncSpec{MaxInFlight: -3}).EffectiveMaxInFlight(); got != 1 {
		t.Errorf("negative EffectiveMaxInFlight = %d, want 1", got)
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

// A namespaced class ID is a legal entity type, and validation must not sanitize
// or reject it.
//
// entity_types[] entries are source-native class IDs compared exactly against the
// tenant's ontology catalog key, so `brick:AHU` and `rec:Space` are ordinary
// values here. The wire half of this invariant is asserted in
// egress.TestClassID_ColonSurvivesTheWire; if the two disagree a kit could declare
// a type it can never receive, or receive one it could not declare.
func TestValidateEgressSyncs_AcceptsNamespacedClassID(t *testing.T) {
	e := EgressSyncSpec{
		Name:           "core-sync",
		ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{
			{Name: "rec:Space", ParentEdges: []string{"hasLocation"}, Hierarchical: true},
			{Name: "brick:Equipment"},
			{Name: "Point"}, // colon-free is equally a class ID
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("namespaced class IDs rejected: %v", err)
	}
	// And the names are carried through unchanged, in declaration order.
	got := e.EntityTypeNames()
	want := []string{"rec:Space", "brick:Equipment", "Point"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EntityTypeNames = %v, want %v verbatim and in order", got, want)
		}
	}
}

// A parent_edges entry must ALREADY be a legal identifier, and a colon is fatal —
// the exact opposite of entity_types[].Name, where a colon is ordinary.
//
// The asymmetry is the point of this test. Both fields hold ontology names, but
// Name is compared against the catalog as an opaque string while a parent_edges entry is
// composed into a type identifier. A declared `rec:hasPart` addresses
// `{tenant}_rec:hasPart`, which has no edges, so the platform's level walk finds
// no children and pushes ONLY THE ROOTS while reporting a complete run — a
// partial push that looks like a successful one.
//
// The platform validates this only when the Day-1 walk reaches it
// (egress.assertAddressableEdge), so without this check the failure is mid-run.
func TestValidateEgressSyncs_RejectsUnaddressableParentEdges(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{
			{Name: "rec:Space", ParentEdges: []string{"rec:hasPart"}},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("a colon-bearing parent_edges entry was accepted; the level walk would silently push only roots")
	}
	// The message must name the form to declare instead — an author who is told
	// only "invalid" has to go read the platform's sanitizer to find out what to
	// write.
	if !strings.Contains(err.Error(), `"rec_hasPart"`) {
		t.Errorf("error must suggest the sanitized form rec_hasPart, got: %v", err)
	}
	if !strings.Contains(err.Error(), "entity_types[0]") {
		t.Errorf("error must locate the offending entity type, got: %v", err)
	}
}

// A trailing underscore is not addressable either, because the sanitizer TRIMS
// trailing underscores rather than preserving them. This is the case the
// platform's first attempt at the same guard missed: it enumerated ":" and "-"
// and would have accepted both this and `has.location`.
func TestValidateEgressSyncs_RejectsParentEdgesCasesACharacterDenylistWouldMiss(t *testing.T) {
	for _, edge := range []string{"hasLocation_", "has.location", "has location", "has/location", "has-location"} {
		e := EgressSyncSpec{
			Name: "core-sync", ExternalSystem: "24k-core",
			EntityTypes: []EgressEntityTypeSpec{{Name: "Location", ParentEdges: []string{edge}}},
			Container:   SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
		}
		if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
			t.Errorf("parent_edges entry %q was accepted but is not what the sanitizer produces", edge)
		}
	}
}

// A parent_edges entry with no legal form at all is reported as such rather than with an
// empty suggestion, which would read as "declare nothing".
func TestValidateEgressSyncs_ParentEdgesWithNoLegalForm(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{{Name: "Location", ParentEdges: []string{":::"}}},
		Container:   SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal(`parent_edges entry ":::" was accepted`)
	}
	if !strings.Contains(err.Error(), "no legal identifier form") {
		t.Errorf("error should say the name has no legal form, got: %v", err)
	}
	if strings.Contains(err.Error(), `declare ""`) {
		t.Errorf("error must not suggest an empty replacement, got: %v", err)
	}
}

// The legal forms stay legal: a bare predicate, an underscored one, and an absent
// one (which means "no containment edge", not "invalid").
func TestValidateEgressSyncs_AcceptsAddressableAndAbsentParentEdges(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{
			{Name: "Location", ParentEdges: []string{"hasLocation"}, Hierarchical: true},
			{Name: "brick:AHU", ParentEdges: []string{"rec_hasPart"}},
			{Name: "Point", ParentEdges: []string{"feeds2", "isPointOf"}},
			{Name: "Equipment"}, // no parent_edges at all
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("addressable / absent parent_edges rejected: %v", err)
	}
}

// sanitizeEdgeName must agree with the platform's ingestion.sanitizeLocalName,
// since the suggestion it produces is only actionable if it names the type the
// platform actually materialized.
func TestSanitizeEdgeName_MatchesPlatformSanitizer(t *testing.T) {
	cases := map[string]string{
		"hasLocation":  "hasLocation",
		"rec:hasPart":  "rec_hasPart",
		"has.location": "has_location",
		"has location": "has_location",
		"hasLocation_": "hasLocation",
		"has__":        "has",
		":::":          "",
		"":             "",
	}
	for in, want := range cases {
		if got := sanitizeEdgeName(in); got != want {
			t.Errorf("sanitizeEdgeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// hierarchical with no parent_edges is incoherent, not a conservative default: a
// level walk would look for roots via an edge that was never named.
func TestValidateEgressSyncs_RejectsHierarchicalWithoutEdge(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{{Name: "Location", Hierarchical: true}},
		Container:   SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("hierarchical with no parent_edges was accepted; the walk has no edge to traverse")
	}
	if !strings.Contains(err.Error(), "parent_edges is empty") {
		t.Errorf("error should name the missing edge, got: %v", err)
	}
}

// A duplicate edge is not merely redundant. Each declared edge becomes one key in
// the entity's parent_refs (see egress.Entity.ParentRefs), so listing it twice
// names one key twice and leaves which resolution wins undefined.
func TestValidateEgressSyncs_RejectsDuplicateParentEdge(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{
			{Name: "Point", ParentEdges: []string{"isPointOf", "isPointOf"}},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("a duplicated parent_edges entry was accepted; it is one parent_refs key twice")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error should say the edge is listed twice, got: %v", err)
	}
}

// An empty list entry is rejected rather than skipped, unlike entity_types[].Name
// where EntityTypeNames() skips blanks. A blank edge cannot resolve anything, so
// silently dropping it would leave a component expecting a parent_refs key that
// never arrives.
func TestValidateEgressSyncs_RejectsEmptyParentEdgeEntry(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{{Name: "Point", ParentEdges: []string{"isPointOf", "  "}}},
		Container:   SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("an empty parent_edges entry was accepted")
	}
}

// include_descendants and hierarchical are INDEPENDENT: a type may select every
// class under its physical type without being a hierarchy, and vice versa.
// Guards against a future "one implies the other" simplification.
func TestValidateEgressSyncs_IncludeDescendantsIsIndependentOfHierarchical(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{
			{Name: "Equipment", IncludeDescendants: true, ParentEdges: []string{"hasLocation"}},
			{Name: "Location", Hierarchical: true, ParentEdges: []string{"hasLocation"}},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("independent include_descendants / hierarchical rejected: %v", err)
	}
}

// A digest-pinned image contains a colon too (`@sha256:`), so the image check must
// not be confused by one. Guards against a future "reject colons" over-correction
// landing on the wrong field.
func TestValidateEgressSyncs_DigestColonIsNotAClassIDColon(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntityTypes: []EgressEntityTypeSpec{{Name: "brick:AHU"}},
		Container:   SidecarContainerSpec{Image: "ghcr.io/ontogisai/x@sha256:deadbeef"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("digest-pinned image with a namespaced type rejected: %v", err)
	}
}
