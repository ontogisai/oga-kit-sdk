package sidecarenv

import "testing"

// TestNamesMatchThePlatform pins each name to the variable the platform's
// Sidecar Manager sets (oga-platform internal/sidecar/manager.go). Renaming one
// here breaks every sidecar that reads it, so a change must start on the
// platform side.
func TestNamesMatchThePlatform(t *testing.T) {
	for got, want := range map[string]string{
		TenantID:        "OGA_TENANT_ID",
		KitID:           "OGA_KIT_ID",
		Component:       "OGA_COMPONENT",
		RegistrationID:  "AGENT_REGISTRATION_ID",
		GatewayURL:      "PLATFORM_GATEWAY_URL",
		BootstrapKind:   "AGENT_BOOTSTRAP_KIND",
		SATokenPath:     "AGENT_SA_TOKEN_PATH",
		BootstrapSecret: "AGENT_BOOTSTRAP_SECRET",
	} {
		if got != want {
			t.Errorf("sidecarenv name %q, want %q", got, want)
		}
	}
}
