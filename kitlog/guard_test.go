package kitlog

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Property 9: No secret leakage (safety invariant).
//
// kitlog must read ONLY the identity/level/format env vars and must never
// reference the bootstrap-secret env names. This guard scans the package's own
// non-test source at test time: every os.Getenv(...) argument must be one of
// the allowlisted Env* constants, and the forbidden secret env names must not
// appear anywhere in the package source.
//
// Validates: Requirements 7.1, 7.2.
func TestProp_NoSecretLeakage(t *testing.T) {
	allowed := map[string]bool{
		"EnvTenantID":       true,
		"EnvRegistrationID": true,
		"EnvKitID":          true,
		"EnvComponent":      true,
		"EnvLogLevel":       true,
		"EnvLogFormat":      true,
	}
	forbidden := []string{"AGENT_BOOTSTRAP_SECRET", "AGENT_SA_TOKEN_PATH"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	getenvRe := regexp.MustCompile(`os\.Getenv\(([A-Za-z0-9_.]+)\)`)

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		scanned++
		src := string(b)

		for _, f := range forbidden {
			if strings.Contains(src, f) {
				t.Errorf("%s references forbidden secret env %q — kitlog must never read credentials", name, f)
			}
		}
		for _, m := range getenvRe.FindAllStringSubmatch(src, -1) {
			if arg := m[1]; !allowed[arg] {
				t.Errorf("%s: os.Getenv(%s) is not in the kitlog env allowlist", name, arg)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no package source files scanned")
	}
}
