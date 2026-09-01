package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fastRetry keeps the retry-path tests quick without disabling retry entirely
// (MaxAttempts 1 would stop them exercising the loop at all).
var fastRetry = RetryConfig{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, BackoffFactor: 2.0}

func TestGet_FetchesBodyAndETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithRetry(fastRetry))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(res.Body) != "payload" {
		t.Errorf("body = %q", res.Body)
	}
	// The quotes ETag values carry are stripped, so a caller can compare directly.
	if res.ETag != "abc123" {
		t.Errorf("etag = %q, want abc123 (quotes stripped)", res.ETag)
	}
}

// TestGet_RejectsNonHTTPS — R6.1. The default must refuse plaintext, since the
// URL this package fetches is customer-supplied.
func TestGet_RejectsNonHTTPS(t *testing.T) {
	d := New(WithRetry(fastRetry))
	for _, raw := range []string{
		"http://example.com/a.json",
		"ftp://example.com/a.json",
		"file:///etc/passwd",
		"//example.com/a.json",
	} {
		_, err := d.Get(context.Background(), raw)
		if err == nil {
			t.Errorf("%q was accepted; only https may be fetched by default", raw)
			continue
		}
		if !IsPermanent(err) && !strings.Contains(err.Error(), "https") && !strings.Contains(err.Error(), "no host") {
			t.Errorf("%q: unexpected error %v", raw, err)
		}
	}
}

// TestGet_EnforcesMaxBytes — R6.2, and permanence matters: an over-cap body will
// not shrink, so retrying it only wastes the budget.
func TestGet_EnforcesMaxBytes(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithMaxBytes(10), WithRetry(fastRetry))
	_, err := d.Get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("an over-cap body was accepted")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error does not mention the cap: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — an over-cap body is permanent and must not be retried", attempts)
	}
}

// TestGet_MaxBytesBoundaryIsInclusive — a body exactly at the cap is valid. The
// implementation reads cap+1 to detect an overrun, so an off-by-one here would
// reject every artifact of exactly the configured size.
func TestGet_MaxBytesBoundaryIsInclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 10)))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithMaxBytes(10), WithRetry(fastRetry))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("a body exactly at the cap was rejected: %v", err)
	}
	if len(res.Body) != 10 {
		t.Errorf("body length = %d, want 10", len(res.Body))
	}
}

// TestGet_HostAllowlist — R6.3. Enforced when set, and the check happens before
// any request so a disallowed host is never contacted at all.
func TestGet_HostAllowlist(t *testing.T) {
	var contacted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted = true
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithRetry(fastRetry),
		WithHostAllowlist([]string{"allowed.example.com"}))
	if _, err := d.Get(context.Background(), srv.URL); err == nil {
		t.Error("a host outside the allowlist was fetched")
	}
	if contacted {
		t.Error("a disallowed host was contacted; the allowlist must be checked before the request")
	}

	// The same server, now allowlisted by its real host, must succeed — otherwise
	// this test would pass against an allowlist that rejects everything.
	host := strings.TrimPrefix(srv.URL, "http://")
	d2 := New(WithAllowInsecure(true), WithRetry(fastRetry), WithHostAllowlist([]string{host}))
	if _, err := d2.Get(context.Background(), srv.URL); err != nil {
		t.Errorf("an allowlisted host was rejected: %v", err)
	}
}

// TestGet_EmptyAllowlistIsUnset — an allowlist that reduces to nothing must mean
// "unset", not "deny everything". A kit reading the list from optional config
// would otherwise ship a connector that can fetch nothing.
func TestGet_EmptyAllowlistIsUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithRetry(fastRetry),
		WithHostAllowlist([]string{"", "   ", ""}))
	if _, err := d.Get(context.Background(), srv.URL); err != nil {
		t.Errorf("an all-empty allowlist behaved as deny-all: %v", err)
	}
}

// TestGet_BearerIsHostScoped — R6.4, the security property of the credential.
// It must reach its own host and no other.
func TestGet_BearerIsHostScoped(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	// Scoped to THIS host (which carries an explicit port, the case a naive
	// Host-vs-Hostname comparison silently gets wrong) — the bearer must attach.
	d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithBearer("tok123", host))
	if _, err := d.Get(context.Background(), srv.URL); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization = %q, want the bearer to attach to its own host "+
			"(a port in the host must not defeat the match)", gotAuth)
	}

	// Scoped to a DIFFERENT host — the bearer must not leak to this origin, which
	// is the presigned-URL case (it authenticates via signed query params).
	gotAuth = ""
	d2 := New(WithAllowInsecure(true), WithRetry(fastRetry), WithBearer("tok123", "other.example.com"))
	if _, err := d2.Get(context.Background(), srv.URL); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty — a credential must not leak to a different host", gotAuth)
	}
}

// TestGet_Retries5xxAndNot4xx — R6.5. The distinction is the whole value of the
// retry policy: a 4xx recurs identically, a 5xx may not.
func TestGet_Retries5xxAndNot4xx(t *testing.T) {
	t.Run("5xx is retried then succeeds", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			if n < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte("recovered"))
		}))
		defer srv.Close()

		d := New(WithAllowInsecure(true), WithRetry(fastRetry))
		res, err := d.Get(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(res.Body) != "recovered" {
			t.Errorf("body = %q", res.Body)
		}
		if n != 3 {
			t.Errorf("attempts = %d, want 3", n)
		}
	})

	t.Run("4xx is permanent", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		d := New(WithAllowInsecure(true), WithRetry(fastRetry))
		_, err := d.Get(context.Background(), srv.URL)
		if err == nil {
			t.Fatal("a 403 was treated as success")
		}
		if n != 1 {
			t.Errorf("attempts = %d, want 1 — a 4xx will recur identically", n)
		}
		if got := StatusOf(err); got != http.StatusForbidden {
			t.Errorf("StatusOf = %d, want 403", got)
		}
	})
}

// TestGet_ErrorRedactsQuery — a presigned URL's signature is a credential and
// must not reach a log through an error string.
func TestGet_ErrorRedactsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithRetry(RetryConfig{MaxAttempts: 1}))
	_, err := d.Get(context.Background(), srv.URL+"/a.json?X-Amz-Signature=SECRETSIG&X-Amz-Expires=900")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SECRETSIG") {
		t.Errorf("error leaked the URL signature: %v", err)
	}
	// The path must survive — a fully-opaque URL would make the error useless for
	// debugging, so redaction has to be surgical rather than total.
	if !strings.Contains(err.Error(), "/a.json") {
		t.Errorf("error dropped the URL path, making it undebuggable: %v", err)
	}
}

// TestSafeURL pins the redaction directly, including the userinfo password case.
// url.Redacted handles that one but NOT the query, so both are asserted here to
// stop a future simplification back to a bare Redacted() call.
func TestSafeURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://h.example.com/a.json":                        "https://h.example.com/a.json",
		"https://h.example.com/a.json?X-Amz-Signature=SEKRIT": "https://h.example.com/a.json?REDACTED",
		"https://h.example.com/a.json#frag":                   "https://h.example.com/a.json",
		"https://user:pass@h.example.com/a.json":              "https://user:xxxxx@h.example.com/a.json",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := safeURL(u); got != want {
			t.Errorf("safeURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestGet_ContextCancellationIsTerminal(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := New(WithAllowInsecure(true), WithRetry(fastRetry))
	if _, err := d.Get(ctx, srv.URL); err == nil {
		t.Error("a cancelled context still produced a successful fetch")
	}
	if n != 0 {
		t.Errorf("attempts = %d, want 0 on a pre-cancelled context", n)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM":            "example.com",
		"  example.com  ":        "example.com",
		"example.com:8443":       "example.com",
		"127.0.0.1:54321":        "127.0.0.1",
		"":                       "",
		"bucket.s3.eu-west-1.io": "bucket.s3.eu-west-1.io",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDo_StopsOnPermanent(t *testing.T) {
	var n int
	err := Do(context.Background(), fastRetry, func() error {
		n++
		return Permanent(fmt.Errorf("nope"))
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
	// The marker is unwrapped on return, so a caller sees its own error.
	if IsPermanent(err) {
		t.Error("Do must return the unwrapped error, not the permanent marker")
	}
}
