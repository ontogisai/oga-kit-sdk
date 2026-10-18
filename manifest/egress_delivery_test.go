package manifest

import (
	"strings"
	"testing"
)

func validDeliverySpec() EgressSyncSpec {
	return EgressSyncSpec{
		Name:           "example-egress",
		ExternalSystem: "ext-system",
		EntitiesSync:   []EgressEntityTypeSpec{{Name: "Equipment"}},
		Container:      SidecarContainerSpec{Image: "ghcr.io/ontogisai/x/y@sha256:abc"},
	}
}

func TestParse_ChangeDeliveryAndSyncGeneration(t *testing.T) {
	src := strings.Replace(egressManifestYAML,
		"      batch_size: 100\n",
		"      batch_size: 100\n      change_delivery: off\n      sync_generation: 3\n", 1)
	m, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := m.Spec.EgressSyncs[0]
	if e.EffectiveChangeDelivery() != ChangeDeliveryOff {
		t.Fatalf("change_delivery = %q, want off", e.ChangeDelivery)
	}
	if e.EffectiveSyncGeneration() != 3 {
		t.Fatalf("sync_generation = %d, want 3", e.EffectiveSyncGeneration())
	}
}

func TestEgress_DeliveryDefaults(t *testing.T) {
	e := validDeliverySpec()
	if got := e.EffectiveChangeDelivery(); got != ChangeDeliveryEvents {
		t.Fatalf("default change_delivery = %q, want events", got)
	}
	if got := e.EffectiveSyncGeneration(); got != 1 {
		t.Fatalf("default sync_generation = %d, want 1", got)
	}
	m, err := Parse(strings.NewReader(egressManifestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Spec.EgressSyncs[0].EffectiveChangeDelivery(); got != ChangeDeliveryEvents {
		t.Fatalf("a manifest without the key reads %q, want events", got)
	}
}

func TestValidateEgressSyncs_Delivery(t *testing.T) {
	gen := func(n int) *int { return &n }
	cases := []struct {
		name    string
		mutate  func(*EgressSyncSpec)
		wantErr string
	}{
		{"omitted", func(*EgressSyncSpec) {}, ""},
		{"events", func(e *EgressSyncSpec) { e.ChangeDelivery = ChangeDeliveryEvents }, ""},
		{"off", func(e *EgressSyncSpec) { e.ChangeDelivery = ChangeDeliveryOff }, ""},
		{"unknown value", func(e *EgressSyncSpec) { e.ChangeDelivery = "batch" }, "change_delivery"},
		{"wrong case is not folded", func(e *EgressSyncSpec) { e.ChangeDelivery = "Off" }, "change_delivery"},
		{"generation 1", func(e *EgressSyncSpec) { e.SyncGeneration = gen(1) }, ""},
		{"generation 7", func(e *EgressSyncSpec) { e.SyncGeneration = gen(7) }, ""},
		{"explicit zero is refused, not read as omitted", func(e *EgressSyncSpec) { e.SyncGeneration = gen(0) }, "sync_generation"},
		{"negative", func(e *EgressSyncSpec) { e.SyncGeneration = gen(-2) }, "sync_generation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := validDeliverySpec()
			c.mutate(&e)
			err := validateEgressSyncs([]EgressSyncSpec{e})
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("error = %v, want one naming %q", err, c.wantErr)
			}
		})
	}
}

// `sync_generation: 0` in YAML reaches validation as an explicit zero, not as
// an omitted field.
func TestParse_ExplicitZeroSyncGenerationIsRefused(t *testing.T) {
	src := strings.Replace(egressManifestYAML,
		"      batch_size: 100\n",
		"      batch_size: 100\n      sync_generation: 0\n", 1)
	m, err := Parse(strings.NewReader(src))
	if err == nil {
		err = Validate(m)
	}
	if err == nil || !strings.Contains(err.Error(), "sync_generation") {
		t.Fatalf("err = %v, want a sync_generation rejection", err)
	}
}
