package manifest

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestParse_BuiltEnvironmentKit_HasPolicies is a cross-repo regression
// guard. It loads the in-tree oga-kit-built-environment manifest (sibling
// directory under the workspace root) and asserts the SDK's new policies
// schema accepts the kit's example block end-to-end. Skipped when the
// sibling repo is not present (e.g., on CI without a multi-repo checkout).
//
// The SDK still has a few known schema gaps relative to the kit (license,
// keywords, target_verticals, loaders) that show up as KnownFields(true)
// rejections via Parse. We tolerate those exactly as the kit's own
// TestManifest_Parses does — what we MUST guarantee here is that
// `policies` does NOT appear in the list of unknown fields, i.e. the SDK
// recognizes the new block.
//
// When the SDK schema closes the rest of the gaps, this test should
// switch to plain ParseFile + Validate.
func TestParse_BuiltEnvironmentKit_HasPolicies(t *testing.T) {
	const path = "../../oga-kit-built-environment/manifest.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("oga-kit-built-environment not present at %s: %v", path, err)
	}

	// Step 1 — strict parse: confirm the parser does not list `policies` as
	// an unknown field. Other known gaps are tolerated.
	if _, err := Parse(bytes.NewReader(data)); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "field policies not found") {
			t.Fatalf("strict parse rejects the new policies field: %v", err)
		}
		// Filter the unknown-field set to confirm only the pre-existing
		// gaps remain. Any new unexpected rejection is a regression.
		var typeErr *yaml.TypeError
		if !errors.As(err, &typeErr) {
			t.Logf("non-TypeError parse failure tolerated: %v", err)
		} else {
			knownGaps := map[string]bool{
				"license":          true,
				"keywords":         true,
				"target_verticals": true,
				"loaders":          true,
			}
			for _, line := range typeErr.Errors {
				if strings.Contains(line, "field policies not found") {
					t.Fatalf("regression: policies still rejected as unknown: %s", line)
				}
				ok := false
				for gap := range knownGaps {
					if strings.Contains(line, "field "+gap+" not found") {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("unexpected unknown-field rejection (new SDK gap?): %s", line)
				}
			}
		}
	}

	// Step 2 — non-strict parse to extract the actual policies and assert
	// they validate cleanly under the SDK's per-entry rules.
	var lenient KitManifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false)
	if err := dec.Decode(&lenient); err != nil {
		t.Fatalf("lenient decode: %v", err)
	}
	if got := len(lenient.Spec.Policies); got < 1 {
		t.Fatalf("expected built-environment kit to ship at least one example policy, got %d", got)
	}
	if err := validateKitPolicies(lenient.Spec.Policies); err != nil {
		t.Fatalf("validateKitPolicies rejected the kit's policies block: %v", err)
	}
}
