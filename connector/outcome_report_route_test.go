package connector

import (
	"net/http"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/outcomereport"
)

// The outcome-report path is RESERVED, so a kit ExtraRoutes entry cannot shadow
// it.
//
// Shadowing would break the contract silently from the platform's side: the
// platform would POST a report and get whatever the kit's handler returned,
// while the kit looked correctly wired. The same reasoning already protects
// /healthz and the webhook path.
func TestReservedPaths_IncludesOutcomeReport(t *testing.T) {
	t.Parallel()
	if !reservedPaths[outcomereport.Path] {
		t.Fatalf("%s is not reserved; a kit route could shadow it", outcomereport.Path)
	}
}

func TestValidateExtraRoutes_RejectsShadowingTheOutcomeReportPath(t *testing.T) {
	t.Parallel()
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	// Both the bare and the method-qualified form must be caught — patternPath
	// strips the method precisely so the qualified form cannot slip through.
	for _, pattern := range []string{
		outcomereport.Path,
		"POST " + outcomereport.Path,
	} {
		err := validateExtraRoutes(map[string]http.Handler{pattern: noop})
		if err == nil {
			t.Errorf("ExtraRoutes[%q] was accepted; it shadows the outcome-report contract path", pattern)
		}
	}
}
