package connector

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Kit-supplied extra routes (OGA-892 / OGA-891 spec R5).
//
// The point of this surface is that needing one extra endpoint must not force a
// kit to abandon connector.ListenAndServe and re-implement the contract by hand.
// The sj24k ontology-sync connector did exactly that -- it needs /trigger and
// /metrics, the mux was closed, so it owns its own http.Server -- and its webhook
// route then drifted from what the platform ingress posts to. That drift is the
// cost this surface exists to remove.
//
// The collision guard is the part worth testing hardest: http.ServeMux PANICS on
// a duplicate registration, so a kit shadowing a reserved contract path has to be
// a named startup error, never a crash in a cluster.

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func TestExtraRoutes_AreServedAlongsideTheContract(t *testing.T) {
	fc := &fakeConnector{bindings: []Binding{{ID: "wo", Mode: ModeWebhook}}}
	s := newTestServer(fc, nil)
	s.cfg.ExtraRoutes = map[string]http.Handler{
		"POST /trigger": okHandler("triggered"),
		"GET /metrics":  okHandler("oga_metric 1"),
	}
	srv := httptest.NewServer(s.mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/trigger", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post /trigger: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "triggered" {
		t.Errorf("/trigger = %d %q", resp.StatusCode, body)
	}

	mresp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	mbody, _ := io.ReadAll(mresp.Body)
	_ = mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK || string(mbody) != "oga_metric 1" {
		t.Errorf("/metrics = %d %q", mresp.StatusCode, mbody)
	}

	// The SDK's own routes must still work — an extra route must not displace the
	// contract it is registered beside.
	lresp, err := http.Get(srv.URL + PathLivez)
	if err != nil {
		t.Fatalf("get livez: %v", err)
	}
	_ = lresp.Body.Close()
	if lresp.StatusCode != http.StatusOK {
		t.Errorf("%s = %d, want 200", PathLivez, lresp.StatusCode)
	}
}

// TestExtraRoutes_RejectsReservedPathCollision — R5.2. Each reserved path is
// checked both bare and method-qualified, because ServeMux patterns carry an
// optional leading METHOD and a guard that compared raw pattern strings would
// miss the qualified form and let ServeMux panic instead.
func TestExtraRoutes_RejectsReservedPathCollision(t *testing.T) {
	for _, pattern := range []string{
		"/healthz",
		"GET /healthz",
		PathLivez,
		"GET " + PathLivez,
		PathTestConnection,
		"POST " + PathTestConnection,
		"/webhook/{binding}",
		"POST /webhook/{binding}",
		"  GET   /healthz  ", // whitespace must not defeat the guard
	} {
		err := validateExtraRoutes(map[string]http.Handler{pattern: okHandler("x")})
		if err == nil {
			t.Errorf("pattern %q was accepted; it collides with a reserved contract path", pattern)
			continue
		}
		// The message must name the offending path — an operator reading a startup
		// failure should not have to diff the pattern list to find it.
		if !strings.Contains(err.Error(), "reserved contract path") {
			t.Errorf("pattern %q: error does not explain the collision: %v", pattern, err)
		}
	}
}

func TestExtraRoutes_AcceptsNonCollidingPatterns(t *testing.T) {
	for _, pattern := range []string{
		"POST /trigger",
		"GET /metrics",
		"GET /healthz-extended", // a PREFIX of no reserved path, and not equal to one
		"GET /webhook",          // the unscoped path is NOT the reserved binding route
		"GET /connector/status",
	} {
		if err := validateExtraRoutes(map[string]http.Handler{pattern: okHandler("x")}); err != nil {
			t.Errorf("pattern %q was rejected: %v", pattern, err)
		}
	}
}

func TestExtraRoutes_RejectsUnusableEntries(t *testing.T) {
	if err := validateExtraRoutes(map[string]http.Handler{"": okHandler("x")}); err == nil {
		t.Error("an empty pattern was accepted")
	}
	if err := validateExtraRoutes(map[string]http.Handler{"GET /x": nil}); err == nil {
		t.Error("a nil handler was accepted")
	}
}

func TestExtraRoutes_NilMapIsFine(t *testing.T) {
	if err := validateExtraRoutes(nil); err != nil {
		t.Errorf("nil ExtraRoutes must be valid: %v", err)
	}
}

func TestPatternPath(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /healthz":       "/healthz",
		"/healthz":           "/healthz",
		"POST   /trigger":    "/trigger",
		"  GET /metrics  ":   "/metrics",
		"/webhook/{binding}": "/webhook/{binding}",
	} {
		if got := patternPath(pattern); got != want {
			t.Errorf("patternPath(%q) = %q, want %q", pattern, got, want)
		}
	}
}
