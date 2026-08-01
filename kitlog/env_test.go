package kitlog

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestInit_InstallsIdentityDefault(t *testing.T) {
	t.Setenv(EnvTenantID, "sgac1")
	t.Setenv(EnvKitID, "built-environment")
	t.Setenv(EnvRegistrationID, "sgac1.fm-operations-agent")
	t.Setenv(EnvComponent, "")

	got := Init()
	if got == nil {
		t.Fatal("Init returned nil")
	}
	if Default() != got {
		t.Fatal("Default() should return the Init()-installed logger")
	}
	if slog.Default() != got {
		t.Fatal("slog.Default() should be the Init()-installed logger")
	}

	// Emit through the seeded logger (write to a buffer via a parallel New with
	// the same env-derived fields — the installed default writes to stdout).
	var buf bytes.Buffer
	lg := New(Options{Writer: &buf, Fields: CommonFieldsFromEnv()})
	lg.Info("started")
	m := parseOneJSON(t, buf.Bytes())
	for _, k := range []string{KeyTenantID, KeyKitID, KeyComponent, KeyRegistrationID} {
		if _, ok := m[k]; !ok {
			t.Fatalf("expected identity field %q in %v", k, m)
		}
	}
	if m[KeyComponent] != "fm-operations-agent" {
		t.Fatalf("component should derive from reg-id suffix, got %v", m[KeyComponent])
	}
}

func TestInit_IdempotentNonNil(t *testing.T) {
	a := Init()
	b := Init()
	if a == nil || b == nil {
		t.Fatal("Init must never return nil")
	}
	if Default() == nil {
		t.Fatal("Default must never return nil")
	}
}

func TestDefault_NonNilWithoutInit(t *testing.T) {
	// Reset the Init-installed base so we exercise the lazy path deterministically.
	baseMu.Lock()
	baseLogger = nil
	baseMu.Unlock()
	if Default() == nil {
		t.Fatal("Default() must be non-nil even when Init() was never called")
	}
}

func TestCommonFieldsFromEnv_ReadsEnv(t *testing.T) {
	t.Setenv(EnvTenantID, "tX")
	t.Setenv(EnvKitID, "kX")
	t.Setenv(EnvComponent, "cX")
	t.Setenv(EnvRegistrationID, "tX.cX")
	attrs := CommonFieldsFromEnv()
	got := map[string]string{}
	for _, a := range attrs {
		got[a.Key] = a.Value.String()
	}
	want := map[string]string{
		KeyTenantID:       "tX",
		KeyKitID:          "kX",
		KeyComponent:      "cX",
		KeyRegistrationID: "tX.cX",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %q, want %q", k, got[k], v)
		}
	}
}
