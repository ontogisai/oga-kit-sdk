package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bigErrorServer answers every request with status and a body far over the
// 512-byte quote cap.
func bigErrorServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(strings.Repeat("x", 200<<10)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertQuoteCapped fails unless err quotes at most 512 bytes of the body and
// says it truncated it (OGA-1036).
func assertQuoteCapped(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, strings.Repeat("x", 513)) {
		t.Errorf("error quotes more than 512 bytes of the body (%d bytes)", len(msg))
	}
	if !strings.Contains(msg, "…(truncated)") {
		t.Errorf("error = %.80q…, want the truncation marker", msg)
	}
}

func TestDoMint_ErrorBodyIsCapped(t *testing.T) {
	srv := bigErrorServer(t, http.StatusForbidden)
	tm := &TokenManager{
		bootstrap:  SecretBootstrap{Secret: "x"},
		issueURL:   srv.URL,
		agentRegID: "t.a",
		client:     srv.Client(),
	}
	_, _, err := tm.doMint(t.Context())
	assertQuoteCapped(t, err)
	if !strings.Contains(err.Error(), "mint returned 403") {
		t.Errorf("error = %.80q…, want the status named", err.Error())
	}
}
