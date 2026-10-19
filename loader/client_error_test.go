package loader_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
)

// loadAgainst calls Load on a loader that answers with status and body, and
// returns the *loader.HTTPError it produced.
func loadAgainst(t *testing.T, status int, body string) *loader.HTTPError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client, err := loader.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Load(t.Context(), &loader.LoadRequest{TenantID: "t1", SourceURI: "file:///x"})
	herr, ok := errors.AsType[*loader.HTTPError](err)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T %v", err, err)
	}
	return herr
}

// The error text quotes at most 512 bytes of the loader's answer (OGA-1036):
// the platform wraps it into its own errors and workflow results.
func TestHTTPError_NonJSONBodyIsCapped(t *testing.T) {
	t.Parallel()
	herr := loadAgainst(t, http.StatusBadGateway, strings.Repeat("x", 4000))
	msg := herr.Error()
	if strings.Contains(msg, strings.Repeat("x", 449)) {
		t.Errorf("Error() quotes more than 448 bytes of the message (%d bytes)", len(msg))
	}
	if !strings.Contains(msg, "…(truncated)") {
		t.Errorf("Error() = %.80q…, want the truncation marker", msg)
	}
}

func TestHTTPError_JSONMessageAndCodeAreCapped(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]string{
		"message": strings.Repeat("m", 4000),
		"code":    strings.Repeat("c", 4000),
	})
	herr := loadAgainst(t, http.StatusInternalServerError, string(body))
	msg := herr.Error()
	if strings.Contains(msg, strings.Repeat("m", 449)) {
		t.Errorf("Error() quotes more than 448 bytes of the message")
	}
	if strings.Contains(msg, strings.Repeat("c", 65)) {
		t.Errorf("Error() quotes more than 64 bytes of the code")
	}
	if !strings.Contains(msg, "(code=") {
		t.Errorf("Error() = %.80q…, want the code quoted", msg)
	}
}

// The error answer is read up to 64 KiB.
func TestHTTPError_BodyReadIsBounded(t *testing.T) {
	t.Parallel()
	herr := loadAgainst(t, http.StatusBadGateway, strings.Repeat("x", 200<<10))
	if herr.Body == nil || len(herr.Body.Message) != 64<<10 {
		got := -1
		if herr.Body != nil {
			got = len(herr.Body.Message)
		}
		t.Errorf("Body.Message holds %d bytes, want the 64 KiB read bound", got)
	}
}

// A short structured answer is quoted as it was before the cap.
func TestHTTPError_ShortAnswerUnchanged(t *testing.T) {
	t.Parallel()
	herr := loadAgainst(t, http.StatusInternalServerError, `{"message":"load failed","code":"load_failed"}`)
	if got, want := herr.Error(), "loader /load: 500 load failed (code=load_failed)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
