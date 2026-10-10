package connector

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// The route is mounted UNCONDITIONALLY — with a nil OutcomeReportReceiver it answers
// 501, never 404.
//
// This is the behaviour the manifest field's documentation now describes, and it
// is load-bearing rather than incidental: the platform reads 501 as "this
// component does not receive reports" and dead-letters immediately, whereas a 404
// is indistinguishable from a misdeployed sidecar and would burn the whole retry
// budget before reporting a misleading transient failure.
//
// Pinned here because the natural implementation is the wrong one — registering
// the route only when a receiver is supplied looks tidier and silently produces
// the 404.
func TestServer_OutcomeReportRouteIsMountedWithoutAReceiver(t *testing.T) {
	t.Parallel()
	s := &server{cfg: &Config{}} // deliberately no OutcomeReportReceiver
	h := s.mux()

	req := httptest.NewRequest(http.MethodPost, outcomereport.Path, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("%s answered 404 with a nil receiver; the platform would retry to "+
			"exhaustion instead of dead-lettering", outcomereport.Path)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 so the platform dead-letters rather than retrying", rec.Code)
	}
}
