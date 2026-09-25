package egress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/outcomereport"
)

// The route is mounted UNCONDITIONALLY — with a nil OutcomeReceiver it answers
// 501, never 404.
//
// The egress twin of the connector's test of the same name, and it exists for the
// same reason: the platform reads 501 as "this component does not receive reports"
// and dead-letters immediately, whereas a 404 is indistinguishable from a
// misdeployed sidecar and would burn the whole retry budget before reporting a
// misleading transient failure.
//
// Pinned on BOTH roles because they are separate packages with separate servers,
// and the whole point of putting the handler in a shared package was to stop the
// two drifting into different answers for the same platform call — which a test on
// only one of them would not catch.
func TestServer_OutcomeReportRouteIsMountedWithoutAReceiver(t *testing.T) {
	t.Parallel()
	s := &server{cfg: &Config{}} // deliberately no OutcomeReceiver
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
