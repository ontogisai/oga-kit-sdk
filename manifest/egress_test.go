package manifest

import (
	"strings"
	"testing"
)

// outEdges builds outbound owner-edge declarations — the shorthand form, and what
// every entry in these tests meant before direction existed (OGA-836). Written as
// a helper rather than inline literals so a test that cares about direction states
// it explicitly, and the rest visibly do not.
func outEdges(names ...string) []ParentEdgeSpec {
	out := make([]ParentEdgeSpec, 0, len(names))
	for _, n := range names {
		out = append(out, ParentEdgeSpec{Edge: n})
	}
	return out
}

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
      entities_sync:
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
	// The scalar shorthand parses to one entry defaulting to OUTBOUND — this is the
	// behavior-preservation check for OGA-836: the YAML above is byte-identical to
	// what a kit wrote before direction existed, and it must still mean out(hasPart).
	if got := e.EntitiesSync[1].ParentEdges; len(got) != 1 || got[0].Edge != "hasPart" {
		t.Errorf("entities_sync[1].parent_edges = %v, want one entry for hasPart", got)
	}
	if got := e.EntitiesSync[1].ParentEdges[0].EffectiveDirection(); got != ParentEdgeOut {
		t.Errorf("scalar shorthand direction = %q, want %q", got, ParentEdgeOut)
	}
	if got := e.EntitiesSync[1].ParentEdges[0].Direction; got != "" {
		t.Errorf("scalar shorthand should leave Direction unset (got %q) so the default is one place", got)
	}
	if !e.EntitiesSync[1].Hierarchical {
		t.Error("entities_sync[1].hierarchical = false, want true")
	}
	if !e.EntitiesSync[1].IncludeDescendants {
		t.Error("entities_sync[1].include_descendants = false, want true")
	}
	// A cross-type reference declares an edge WITHOUT hierarchical: it needs the
	// owner resolved but no level walk.
	if e.EntitiesSync[2].Hierarchical {
		t.Error("entities_sync[2].hierarchical = true; a cross-type edge is not a hierarchy")
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
	e := &EgressSyncSpec{EntitiesSync: []EgressEntityTypeSpec{{Name: "A"}, {Name: ""}, {Name: "B"}}}
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
			EntitiesSync:   []EgressEntityTypeSpec{{Name: "Equipment"}},
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
		{"no entity types", func(e *EgressSyncSpec) { e.EntitiesSync = nil }, "at least one entities_sync entry"},
		{
			"entity types all unnamed",
			func(e *EgressSyncSpec) { e.EntitiesSync = []EgressEntityTypeSpec{{}, {}} },
			"at least one entities_sync entry",
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
		EntitiesSync:   []EgressEntityTypeSpec{{Name: "NoSuchTypeAnywhere"}},
		Container:      SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Errorf("unknown entity type rejected locally: %v — the ontology check is install-time only", err)
	}
}

// A namespaced class ID is a legal entity type, and validation must not sanitize
// or reject it.
//
// entities_sync[] entries are source-native class IDs compared exactly against the
// tenant's ontology catalog key, so `brick:AHU` and `rec:Space` are ordinary
// values here. The wire half of this invariant is asserted in
// egress.TestClassID_ColonSurvivesTheWire; if the two disagree a kit could declare
// a type it can never receive, or receive one it could not declare.
func TestValidateEgressSyncs_AcceptsNamespacedClassID(t *testing.T) {
	e := EgressSyncSpec{
		Name:           "core-sync",
		ExternalSystem: "24k-core",
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "rec:Space", ParentEdges: outEdges("hasLocation"), Hierarchical: true},
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
// the exact opposite of entities_sync[].Name, where a colon is ordinary.
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
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "rec:Space", ParentEdges: outEdges("rec:hasPart")},
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
	if !strings.Contains(err.Error(), "entities_sync[0]") {
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
			EntitiesSync: []EgressEntityTypeSpec{{Name: "Location", ParentEdges: outEdges(edge)}},
			Container:    SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
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
		EntitiesSync: []EgressEntityTypeSpec{{Name: "Location", ParentEdges: outEdges(":::")}},
		Container:    SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
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
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "Location", ParentEdges: outEdges("hasLocation"), Hierarchical: true},
			{Name: "brick:AHU", ParentEdges: outEdges("rec_hasPart")},
			{Name: "Point", ParentEdges: outEdges("feeds2", "isPointOf")},
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
		EntitiesSync: []EgressEntityTypeSpec{{Name: "Location", Hierarchical: true}},
		Container:    SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
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
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "Point", ParentEdges: outEdges("isPointOf", "isPointOf")},
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

// An empty list entry is rejected rather than skipped, unlike entities_sync[].Name
// where EntityTypeNames() skips blanks. A blank edge cannot resolve anything, so
// silently dropping it would leave a component expecting a parent_refs key that
// never arrives.
func TestValidateEgressSyncs_RejectsEmptyParentEdgeEntry(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntitiesSync: []EgressEntityTypeSpec{{Name: "Point", ParentEdges: outEdges("isPointOf", "  ")}},
		Container:    SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
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
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "Equipment", IncludeDescendants: true, ParentEdges: outEdges("hasLocation")},
			{Name: "Location", Hierarchical: true, ParentEdges: outEdges("hasLocation")},
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
		EntitiesSync: []EgressEntityTypeSpec{{Name: "brick:AHU"}},
		Container:    SidecarContainerSpec{Image: "ghcr.io/ontogisai/x@sha256:deadbeef"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("digest-pinned image with a namespaced type rejected: %v", err)
	}
}

// ─── Owner-edge direction (OGA-836) ─────────────────────────────────────────
//
// Direction exists because an owner edge is not always stored child-to-parent. A
// loader that normalizes inverse predicates to one canonical edge decides the
// stored orientation, and for "has a point / has a part" style predicates that
// orientation is parent-to-child — so the owner sits on the in() side. Before this
// field such a relation could not be declared at all: naming the inverse predicate
// fails install (no such relationship type) and naming the stored edge resolves
// zero owners, which reads as "root" and pushes every record unowned while
// reporting success.

// egressManifestWithEntitiesSync wraps an entities_sync block in an otherwise valid
// manifest, so a direction test states only the part it is about.
func egressManifestWithEntitiesSync(entityTypes string) string {
	return `api_version: ontogis.ai/v1
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
    - name: core-egress-sync
      external_system: 24k-core
      entities_sync:
` + entityTypes + `      container:
        image: ghcr.io/ontogisai/oga-kit-sj24k/core-egress@sha256:abc123
`
}

// The mapping form parses, and a list may MIX shorthand and mapping entries — the
// realistic shape, since a kit typically has one inbound edge among several
// outbound ones and should not have to convert the others.
func TestParse_ParentEdgesMappingFormAndMixedList(t *testing.T) {
	y := egressManifestWithEntitiesSync(`        - name: Location
          parent_edges: [hasLocation]
          hierarchical: true
        - name: Point
          parent_edges:
            - hasLocation
            - edge: hasPoint
              direction: in
`)
	m, err := Parse(strings.NewReader(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	types := m.Spec.EgressSyncs[0].EntitiesSync

	if got := types[0].ParentEdges[0].EffectiveDirection(); got != ParentEdgeOut {
		t.Errorf("shorthand entry direction = %q, want %q", got, ParentEdgeOut)
	}

	mixed := types[1].ParentEdges
	if len(mixed) != 2 {
		t.Fatalf("mixed list parsed to %d entries, want 2", len(mixed))
	}
	if mixed[0].Edge != "hasLocation" || mixed[0].EffectiveDirection() != ParentEdgeOut {
		t.Errorf("mixed[0] = %+v, want hasLocation/out", mixed[0])
	}
	if mixed[1].Edge != "hasPoint" || mixed[1].EffectiveDirection() != ParentEdgeIn {
		t.Errorf("mixed[1] = %+v, want hasPoint/in", mixed[1])
	}
}

// A mistyped direction key must be REJECTED, not ignored.
//
// This is the sharpest test in the file, because the failure it guards is one the
// obvious implementation reintroduces. yaml.v3's Node.Decode does not inherit the
// parent decoder's KnownFields setting, so a ParentEdgeSpec that unmarshalled via a
// shadow struct would silently drop "directon" — leaving Direction empty, defaulting
// to OUTBOUND, and producing the exact silent wrong-direction push the field exists
// to prevent. Worse, it would do so in the one place the author was being explicit.
func TestParse_ParentEdgesRejectsUnknownFieldInMapping(t *testing.T) {
	y := egressManifestWithEntitiesSync(`        - name: Point
          parent_edges:
            - edge: hasPoint
              directon: in
`)
	_, err := Parse(strings.NewReader(y))
	if err == nil {
		t.Fatal("a mistyped direction key was accepted; it would silently default to outbound")
	}
	if !strings.Contains(err.Error(), "directon") {
		t.Errorf("error must name the unknown field, got: %v", err)
	}
}

// Anything that is neither an edge name nor an edge/direction mapping is rejected
// with a message that says what the two legal shapes are.
func TestParse_ParentEdgesRejectsUnsupportedNodeShape(t *testing.T) {
	y := egressManifestWithEntitiesSync(`        - name: Point
          parent_edges:
            - [hasPoint, in]
`)
	_, err := Parse(strings.NewReader(y))
	if err == nil {
		t.Fatal("a sequence parent_edges entry was accepted")
	}
	if !strings.Contains(err.Error(), "edge/direction") {
		t.Errorf("error should name the legal shapes, got: %v", err)
	}
}

// An unrecognized direction is rejected rather than normalized. "inbound" is the
// plausible wrong guess, and silently treating it as outbound would invert the
// author's stated intent.
func TestValidateEgressSyncs_RejectsUnknownDirection(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "Point", ParentEdges: []ParentEdgeSpec{{Edge: "hasPoint", Direction: "inbound"}}},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal(`direction "inbound" was accepted; it is not a traversal the platform emits`)
	}
	for _, want := range []string{`"inbound"`, `"out"`, `"in"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %s, got: %v", want, err)
		}
	}
}

// The same edge in both directions collides: both resolve into the ONE parent_refs
// key named by the edge, so which owner wins is undefined. It is a distinct failure
// from the plain duplicate because the remedy differs — decide which way the edge
// is stored, rather than delete a redundant line.
func TestValidateEgressSyncs_RejectsSameEdgeInBothDirections(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "Point", ParentEdges: []ParentEdgeSpec{
				{Edge: "hasPoint"},
				{Edge: "hasPoint", Direction: ParentEdgeIn},
			}},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("the same edge in both directions was accepted; both are one parent_refs key")
	}
	if !strings.Contains(err.Error(), "both directions") {
		t.Errorf("error should name the collision, got: %v", err)
	}
	// It must NOT be reported as a plain duplicate: that would send the author to
	// delete a line rather than to establish the stored orientation.
	if strings.Contains(err.Error(), "twice") {
		t.Errorf("both-directions must not be reported as a plain duplicate, got: %v", err)
	}
}

// An inbound declaration is otherwise ordinary: it is accepted, and the edge-name
// addressability rule still applies to it. Direction changes how the platform
// traverses the edge, not what a legal edge name looks like.
func TestValidateEgressSyncs_InboundDirection(t *testing.T) {
	base := func(edges []ParentEdgeSpec) []EgressSyncSpec {
		return []EgressSyncSpec{{
			Name: "core-sync", ExternalSystem: "24k-core",
			EntitiesSync: []EgressEntityTypeSpec{{Name: "Point", ParentEdges: edges}},
			Container:    SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
		}}
	}
	if err := validateEgressSyncs(base([]ParentEdgeSpec{
		{Edge: "hasPoint", Direction: ParentEdgeIn},
	})); err != nil {
		t.Fatalf("an inbound owner edge was rejected: %v", err)
	}
	err := validateEgressSyncs(base([]ParentEdgeSpec{
		{Edge: "rec:hasPoint", Direction: ParentEdgeIn},
	}))
	if err == nil {
		t.Fatal("an unaddressable edge was accepted under direction: in")
	}
	if !strings.Contains(err.Error(), `"rec_hasPoint"`) {
		t.Errorf("error should still suggest the sanitized form, got: %v", err)
	}
}

// A hierarchical type must declare EXACTLY one edge. "Level" is hops along a single
// containment axis, so two declared edges leave a row's depth undefined and there
// is no non-arbitrary way to pick the axis. The platform's installer already
// rejected this; the SDK did not, so a kit author got a local pass and an install
// failure (parity fix found while implementing OGA-836).
func TestValidateEgressSyncs_RejectsHierarchicalWithSeveralEdges(t *testing.T) {
	e := EgressSyncSpec{
		Name: "core-sync", ExternalSystem: "24k-core",
		EntitiesSync: []EgressEntityTypeSpec{
			{Name: "Location", Hierarchical: true, ParentEdges: outEdges("managedBy", "hasLocation")},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/e@sha256:abc"},
	}
	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("hierarchical with two parent_edges was accepted; the hierarchy axis is ambiguous")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error should say exactly one edge is required, got: %v", err)
	}
	// A NON-hierarchical type may still declare several: it wants each owner
	// referenced but no level walk, so there is no axis to disambiguate.
	e.EntitiesSync[0].Hierarchical = false
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("several edges without hierarchical were rejected: %v", err)
	}
}

// HierarchyEdge is the single resolution point for "which edge orders the walk, and
// which way". It reports the direction RESOLVED, so a caller cannot accidentally
// walk an inbound hierarchy outbound by reading the raw zero value.
func TestHierarchyEdge(t *testing.T) {
	t.Run("resolves the declared direction", func(t *testing.T) {
		et := &EgressEntityTypeSpec{
			Name: "Assembly", Hierarchical: true,
			ParentEdges: []ParentEdgeSpec{{Edge: "hasPart", Direction: ParentEdgeIn}},
		}
		got, ok := et.HierarchyEdge()
		if !ok {
			t.Fatal("HierarchyEdge reported no hierarchy for a hierarchical type")
		}
		if got.Edge != "hasPart" || got.Direction != ParentEdgeIn {
			t.Errorf("HierarchyEdge = %+v, want hasPart/in", got)
		}
	})

	t.Run("defaults an unset direction rather than returning the zero value", func(t *testing.T) {
		et := &EgressEntityTypeSpec{
			Name: "Location", Hierarchical: true, ParentEdges: outEdges("hasLocation"),
		}
		got, ok := et.HierarchyEdge()
		if !ok {
			t.Fatal("HierarchyEdge reported no hierarchy")
		}
		if got.Direction != ParentEdgeOut {
			t.Errorf("direction = %q, want it resolved to %q", got.Direction, ParentEdgeOut)
		}
	})

	t.Run("no hierarchy when not declared or ambiguous", func(t *testing.T) {
		flat := &EgressEntityTypeSpec{Name: "Equipment", ParentEdges: outEdges("hasLocation")}
		if _, ok := flat.HierarchyEdge(); ok {
			t.Error("a non-hierarchical type reported a hierarchy edge")
		}
		ambiguous := &EgressEntityTypeSpec{
			Name: "Location", Hierarchical: true, ParentEdges: outEdges("a", "b"),
		}
		if _, ok := ambiguous.HierarchyEdge(); ok {
			t.Error("an ambiguous multi-edge hierarchy reported one edge; validation rejects this shape")
		}
		var nilType *EgressEntityTypeSpec
		if _, ok := nilType.HierarchyEdge(); ok {
			t.Error("a nil type reported a hierarchy edge")
		}
	})
}

func TestNormalizeDirectionAndValid(t *testing.T) {
	if got := NormalizeDirection(""); got != ParentEdgeOut {
		t.Errorf("NormalizeDirection(\"\") = %q, want %q — an omitted direction must mean what it always meant", got, ParentEdgeOut)
	}
	if got := NormalizeDirection(ParentEdgeIn); got != ParentEdgeIn {
		t.Errorf("NormalizeDirection(in) = %q", got)
	}
	for _, d := range []ParentEdgeDirection{"", ParentEdgeOut, ParentEdgeIn} {
		if !d.Valid() {
			t.Errorf("direction %q should be valid", d)
		}
	}
	for _, d := range []ParentEdgeDirection{"inbound", "outbound", "IN", "both"} {
		if d.Valid() {
			t.Errorf("direction %q should be invalid", d)
		}
	}
}

// ParentEdgeNames is direction-blind on purpose, for callers that only need the
// names. It skips blanks, matching EntityTypeNames.
func TestParentEdgeNames(t *testing.T) {
	et := &EgressEntityTypeSpec{ParentEdges: []ParentEdgeSpec{
		{Edge: "hasLocation"},
		{Edge: "hasPoint", Direction: ParentEdgeIn},
		{Edge: ""},
	}}
	got := et.ParentEdgeNames()
	want := []string{"hasLocation", "hasPoint"}
	if len(got) != len(want) {
		t.Fatalf("ParentEdgeNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParentEdgeNames = %v, want %v in declared order", got, want)
		}
	}
	var empty *EgressEntityTypeSpec
	if got := (&EgressEntityTypeSpec{}).ParentEdgeNames(); got != nil {
		t.Errorf("no edges should yield nil, got %v", got)
	}
	_ = empty
}

// --- Ontology-type catalog lane (OGA-845) ---

// ontologyLaneManifestYAML is what a kit author writes for a component with BOTH
// lanes. It is the worked shape from the design: one anchor whose external target
// has a parent foreign key (so the selection closes over parents) and one flat
// anchor, plus entity types that reference the catalog via type_ref.
const ontologyLaneManifestYAML = `api_version: ontogis.ai/v1
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
    - name: core-egress-sync
      external_system: 24k-core
      ontology_sync:
        - anchor: Equipment
          include_parents: true
        - anchor: Point
          include_parents: false
      entities_sync:
        - name: Equipment
          parent_edges: [hasLocation]
          include_descendants: true
          type_ref: true
        - name: Point
          parent_edges:
            - edge: hasPoint
              direction: in
          include_descendants: true
          type_ref: true
      container:
        image: ghcr.io/ontogisai/oga-kit-sj24k/core-egress@sha256:abc123
`

// The lane parses through the STRICT decoder — which is the check that matters,
// since manifest.Parse sets KnownFields(true), so before this field existed the
// same input was a hard parse error and a kit author could not lint it at all.
func TestParse_OntologySyncLane(t *testing.T) {
	m, err := Parse(strings.NewReader(ontologyLaneManifestYAML))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	e := &m.Spec.EgressSyncs[0]

	// Declared order is preserved. It is not the push order WITHIN the lane — the
	// platform resolves each anchor's population and walks it by containment depth —
	// but the lane as a whole precedes the entities lane, and an author reading the
	// parsed spec back should see what they wrote.
	if got, want := e.OntologyAnchors(), []string{"Equipment", "Point"}; len(got) != len(want) {
		t.Fatalf("OntologyAnchors = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("OntologyAnchors = %v, want %v in declared order", got, want)
			}
		}
	}
	if !e.OntologySync[0].IncludeParents {
		t.Error("ontology_sync[0].include_parents = false, want true (the target has a parent FK)")
	}
	// An explicit `false` and an omitted key must both mean "flat". Asserted because
	// the omitempty tag makes the two indistinguishable once parsed, so a future
	// change to a tri-state would silently alter what `false` means.
	if e.OntologySync[1].IncludeParents {
		t.Error("ontology_sync[1].include_parents = true, want false (the target is flat)")
	}
	if !e.EntitiesSync[0].TypeRef || !e.EntitiesSync[1].TypeRef {
		t.Error("type_ref did not parse on the entities lane")
	}
}

// The two lanes are INDEPENDENT: a component may declare entities only, which is
// every pre-OGA-845 kit, and must keep working unchanged.
func TestParse_EntitiesLaneWithoutOntologyLane(t *testing.T) {
	m, err := Parse(strings.NewReader(egressManifestYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(m); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := m.Spec.EgressSyncs[0].OntologyAnchors(); got != nil {
		t.Errorf("OntologyAnchors = %v, want nil when no lane is declared", got)
	}
}

// The RENAME is the reason this work item is breaking, and this is the test that
// proves the break is loud.
//
// A kit that keeps writing `entity_types` must be REJECTED at parse time, not
// silently decoded into an empty EntitiesSync. Silent would be the worst outcome
// available: the component would deploy, push nothing, and report a clean run —
// which is indistinguishable from a tenant with no data.
func TestParse_LegacyEntityTypesKeyIsRejected(t *testing.T) {
	legacy := strings.Replace(ontologyLaneManifestYAML, "entities_sync:", "entity_types:", 1)
	if legacy == ontologyLaneManifestYAML {
		t.Fatal("test setup: the entities_sync key was not found, so nothing was renamed")
	}
	_, err := Parse(strings.NewReader(legacy))
	if err == nil {
		t.Fatal("the legacy entity_types key was accepted; the strict decoder must reject it, " +
			"because a silently dropped block pushes nothing and reports success")
	}
	// The message must name the offending key — that is what turns the rejection
	// into a migration instruction rather than a puzzle.
	if !strings.Contains(err.Error(), "entity_types") {
		t.Errorf("rejection should name the unknown field, got: %v", err)
	}
}

// anchor is required, and the error names the exact path so an author can find it
// in a manifest with several components and several anchors.
func TestValidateEgressSyncs_OntologySyncRequiresAnchor(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = []EgressOntologySyncSpec{{Anchor: "Equipment"}, {IncludeParents: true}}

	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("an ontology_sync entry with no anchor was accepted")
	}
	if !strings.Contains(err.Error(), "ontology_sync[1]") || !strings.Contains(err.Error(), "anchor is required") {
		t.Errorf("error should name ontology_sync[1] and the missing field, got: %v", err)
	}
}

// Whitespace is not an anchor. Checked separately because TrimSpace is the kind of
// thing an equivalent implementation omits, and " " would then pass validation and
// select nothing at run time.
func TestValidateEgressSyncs_OntologySyncBlankAnchorIsNotAnAnchor(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = []EgressOntologySyncSpec{{Anchor: "   "}}

	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("a whitespace-only anchor was accepted")
	}
}

// A repeated anchor is the SAME push twice, not a bigger one: the population is
// resolved from the anchor, so every row would be pushed and correlated again. When
// the two entries disagree on include_parents it is worse still — which closure
// applies would be decided by evaluation order rather than by the declaration.
func TestValidateEgressSyncs_OntologySyncRejectsDuplicateAnchor(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = []EgressOntologySyncSpec{
		{Anchor: "Equipment", IncludeParents: true},
		{Anchor: "Equipment"},
	}

	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("a duplicate anchor was accepted")
	}
	if !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("error should say the anchor duplicates an earlier entry, got: %v", err)
	}
}

// Two anchors are the normal case — that is the whole reason the lane is a list.
func TestValidateEgressSyncs_OntologySyncAcceptsSeveralAnchors(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = []EgressOntologySyncSpec{
		{Anchor: "Equipment", IncludeParents: true},
		{Anchor: "Point"},
	}

	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("two distinct anchors should be valid: %v", err)
	}
}

// type_ref with no ontology lane at all is rejected locally, because that verdict
// needs no tenant state: with no catalog pushed there is nothing to reference
// whichever anchor the type turns out to live under.
//
// The per-anchor half of the guard is deliberately NOT here — resolving which
// anchor stores a given type needs the tenant's ontology, so the platform makes
// that call at install. Guessing locally would reject valid manifests.
func TestValidateEgressSyncs_TypeRefRequiresAnOntologyLane(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = nil
	e.EntitiesSync = []EgressEntityTypeSpec{{Name: "Equipment", TypeRef: true}}

	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("type_ref with no ontology_sync lane was accepted; every batch would fail at run time")
	}
	if !strings.Contains(err.Error(), "type_ref") || !strings.Contains(err.Error(), "ontology_sync") {
		t.Errorf("error should name both type_ref and the missing lane, got: %v", err)
	}
	if !strings.Contains(err.Error(), "entities_sync[0]") {
		t.Errorf("error should name the offending entities_sync entry, got: %v", err)
	}
}

// The mirror image: type_ref is fine once a lane exists. Asserted so the guard
// above cannot be over-tightened into "type_ref is never allowed".
func TestValidateEgressSyncs_TypeRefWithALaneIsAccepted(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = []EgressOntologySyncSpec{{Anchor: "Equipment", IncludeParents: true}}
	e.EntitiesSync = []EgressEntityTypeSpec{{Name: "Equipment", TypeRef: true}}

	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("type_ref alongside a declared lane should be valid: %v", err)
	}
}

// An ontology lane does not substitute for the entities lane. Kept explicit
// because it is the one place where adding a lane could plausibly have relaxed an
// existing rule, and the platform applies the same rule — so accepting it here
// would break the local/install parity this validation exists for.
func TestValidateEgressSyncs_OntologyLaneDoesNotSatisfyTheEntitiesRequirement(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.OntologySync = []EgressOntologySyncSpec{{Anchor: "Equipment"}}
	e.EntitiesSync = nil

	err := validateEgressSyncs([]EgressSyncSpec{e})
	if err == nil {
		t.Fatal("a component with only an ontology lane was accepted")
	}
	if !strings.Contains(err.Error(), "entities_sync") {
		t.Errorf("error should name the missing lane, got: %v", err)
	}
}

// OntologyAnchors skips empty entries, matching EntityTypeNames. Validation rejects
// those entries anyway, so this only pins the two helpers to the same convention.
func TestOntologyAnchors_SkipsEmptyAndPreservesOrder(t *testing.T) {
	e := &EgressSyncSpec{OntologySync: []EgressOntologySyncSpec{
		{Anchor: "Equipment"}, {Anchor: ""}, {Anchor: "Point"},
	}}
	got, want := e.OntologyAnchors(), []string{"Equipment", "Point"}
	if len(got) != len(want) {
		t.Fatalf("OntologyAnchors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OntologyAnchors = %v, want %v in declared order", got, want)
		}
	}
	if got := (&EgressSyncSpec{}).OntologyAnchors(); got != nil {
		t.Errorf("no lane should yield nil, got %v", got)
	}
}

// validEgressSpecForOntologyLane is a component that passes every check, so each
// test above mutates exactly the field it is about.
func validEgressSpecForOntologyLane() EgressSyncSpec {
	return EgressSyncSpec{
		Name:           "core-egress-sync",
		ExternalSystem: "24k-core",
		EntitiesSync:   []EgressEntityTypeSpec{{Name: "Equipment"}},
		Container:      SidecarContainerSpec{Image: "ghcr.io/x/egress@sha256:abc123"},
	}
}

// validEgressSpecForRelationshipLane is a component that passes every check, so
// each relationship-lane test below mutates exactly the field it is about.
func validEgressSpecForRelationshipLane() EgressSyncSpec {
	return EgressSyncSpec{
		Name:           "core-egress-sync",
		ExternalSystem: "24k-core",
		EntitiesSync:   []EgressEntityTypeSpec{{Name: "Equipment"}, {Name: "Location"}},
		RelationshipsSync: []EgressRelationshipSyncSpec{
			{Predicate: "feeds", SourceType: "Equipment", TargetType: "Equipment"},
			{Predicate: "feeds", SourceType: "Equipment", TargetType: "Location", IncludeDescendants: true},
		},
		Container: SidecarContainerSpec{Image: "ghcr.io/x/egress@sha256:abc123"},
	}
}

// A well-formed relationships_sync declaration, including two entries sharing a
// predicate but scoped to different endpoint pairs (the exact shape OGA-875's
// design requires for `feeds`), passes.
func TestValidateEgressSyncs_RelationshipSyncAccepted(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("valid relationships_sync declaration rejected: %v", err)
	}
}

// predicate is required — there is no default, since it both selects the read
// and identifies the batch's homogeneity label.
func TestValidateEgressSyncs_RelationshipSyncRequiresPredicate(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	e.RelationshipsSync = []EgressRelationshipSyncSpec{{SourceType: "Equipment", TargetType: "Equipment"}}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("expected an error for a relationship entry with no predicate")
	}
}

// source_type is required — a relationship entry must scope BOTH endpoints, since
// one predicate name can span several distinct anchor pairs (feeds spans
// Equipment->Equipment and Equipment->Location in the sj24k campus export).
func TestValidateEgressSyncs_RelationshipSyncRequiresSourceType(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	e.RelationshipsSync = []EgressRelationshipSyncSpec{{Predicate: "feeds", TargetType: "Equipment"}}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("expected an error for a relationship entry with no source_type")
	}
}

// target_type is required, the mirror image of source_type above.
func TestValidateEgressSyncs_RelationshipSyncRequiresTargetType(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	e.RelationshipsSync = []EgressRelationshipSyncSpec{{Predicate: "feeds", SourceType: "Equipment"}}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("expected an error for a relationship entry with no target_type")
	}
}

// Blank (whitespace-only) fields are not a legal way to satisfy the required-field
// check — the same discipline the ontology anchor check applies.
func TestValidateEgressSyncs_RelationshipSyncBlankFieldsAreNotValues(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	e.RelationshipsSync = []EgressRelationshipSyncSpec{{Predicate: "feeds", SourceType: "  ", TargetType: "Equipment"}}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("expected an error for a relationship entry with a blank source_type")
	}
}

// A repeated (predicate, source_type, target_type) tuple is the SAME push twice,
// not a bigger one — the tuple resolves the whole read scope, so a duplicate is
// rejected the same way a duplicate ontology anchor is.
func TestValidateEgressSyncs_RelationshipSyncRejectsDuplicateTuple(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	e.RelationshipsSync = []EgressRelationshipSyncSpec{
		{Predicate: "feeds", SourceType: "Equipment", TargetType: "Equipment"},
		{Predicate: "feeds", SourceType: "Equipment", TargetType: "Equipment"},
	}
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err == nil {
		t.Fatal("expected an error for a duplicate (predicate, source_type, target_type) tuple")
	}
}

// Two entries sharing a predicate but scoped to DIFFERENT target types are not a
// duplicate — this is the exact shape the design requires for `feeds`
// (Equipment->Equipment vs Equipment->Location) and must be accepted.
func TestValidateEgressSyncs_RelationshipSyncSamePredicateDifferentTargetIsNotADuplicate(t *testing.T) {
	e := validEgressSpecForRelationshipLane() // already carries this exact shape
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("two entries sharing a predicate but scoped to different target types must be accepted: %v", err)
	}
}

// An absent relationships_sync block is legal — the lane is entirely optional, and
// a component declaring none must not be rejected for it.
func TestValidateEgressSyncs_RelationshipSyncAbsentIsAccepted(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	e.RelationshipsSync = nil
	if err := validateEgressSyncs([]EgressSyncSpec{e}); err != nil {
		t.Fatalf("a component declaring no relationships_sync must be accepted: %v", err)
	}
}

// RelationshipEntityTypes returns the distinct source_type/target_type names in
// declared order, deduplicated — what the platform's convergence gate
// (OGA-EGRS-VAL-1008) reads to decide which entity types must have converged
// before a relationships-scoped run may proceed.
func TestEgressSyncSpec_RelationshipEntityTypes(t *testing.T) {
	e := validEgressSpecForRelationshipLane()
	got := e.RelationshipEntityTypes()
	want := []string{"Equipment", "Location"} // Equipment appears 3x across both entries, deduplicated
	if len(got) != len(want) {
		t.Fatalf("RelationshipEntityTypes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RelationshipEntityTypes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// No relationships_sync declared ⇒ no entity types to converge-check, not a panic
// or a spurious empty-but-non-nil slice that a caller might mistake for "declared,
// but empty".
func TestEgressSyncSpec_RelationshipEntityTypesAbsentIsNil(t *testing.T) {
	e := validEgressSpecForOntologyLane()
	if got := e.RelationshipEntityTypes(); got != nil {
		t.Errorf("RelationshipEntityTypes() = %v, want nil", got)
	}
}
