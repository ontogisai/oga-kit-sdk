package loader

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMaterializeRemoteSource_HTTPDownloadsToFile(t *testing.T) {
	const body = `{"classes":["Building","Floor"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	req := &LoadRequest{SourceURI: srv.URL + "/brick-classes.json"}
	cleanup, err := materializeRemoteSource(t.Context(), req)
	if err != nil {
		t.Fatalf("materializeRemoteSource: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(req.SourceURI, "file://") {
		t.Fatalf("SourceURI not rewritten to file://: %q", req.SourceURI)
	}
	path := strings.TrimPrefix(req.SourceURI, "file://")
	// Test-only: path is the temp file materializeRemoteSource just created, and
	// reading it back IS the assertion, so no untrusted input reaches the call.
	// The semgrep suppression must sit on the sink line to take effect.
	got, err := os.ReadFile(path) //nolint:gosec // test temp path -- nosemgrep: gosec.G304-1
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(got) != body {
		t.Errorf("materialized content = %q, want %q", got, body)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp file not removed by cleanup: %v", err)
	}
}

func TestMaterializeRemoteSource_FileSchemeIsNoop(t *testing.T) {
	req := &LoadRequest{SourceURI: "file:///var/lib/oga/kits/x/1.0.0/data.json"}
	cleanup, err := materializeRemoteSource(t.Context(), req)
	if err != nil {
		t.Fatalf("materializeRemoteSource: %v", err)
	}
	defer cleanup()
	if req.SourceURI != "file:///var/lib/oga/kits/x/1.0.0/data.json" {
		t.Errorf("file:// SourceURI should be unchanged, got %q", req.SourceURI)
	}
}

func TestMaterializeRemoteSource_HTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	req := &LoadRequest{SourceURI: srv.URL + "/nope"}
	_, err := materializeRemoteSource(t.Context(), req)
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}
