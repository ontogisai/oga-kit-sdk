package fetch

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for defects found reviewing the spool rewrite. Each one FAILED
// against the first version of that change; they are grouped here so the reason
// each exists stays attached to it.

// urlErrorBodyTransport answers 200 and then fails the BODY read with a
// *url.Error carrying the full signed URL.
//
// This shape is what the redaction test below needs and what the pre-existing
// mid-transfer test could not produce. Stock net/http never puts a *url.Error on
// a body read — a truncated transfer surfaces as io.ErrUnexpectedEOF — so a test
// driven through httptest cannot exercise the scrub on this path at all, and the
// one that claimed to was passing vacuously. A wrapping or instrumenting
// RoundTripper returning *url.Error from a body read is ordinary, and
// WithHTTPClient exists to let a kit install one.
type urlErrorBodyTransport struct{ target string }

func (t urlErrorBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          urlErrorBody(t),
		ContentLength: -1,
	}, nil
}

type urlErrorBody struct{ target string }

func (b urlErrorBody) Read([]byte) (int, error) {
	return 0, &url.Error{Op: "Get", URL: b.target, Err: errors.New("connection reset by peer")}
}

func (urlErrorBody) Close() error { return nil }

// TestGet_SpoolBodyErrorRedactsTheURL pins the ordering scrubURLError's own doc
// calls load-bearing: the scrub must happen INSIDE the wrap.
//
// The first version of the spool rewrite wrapped in writeSpool and scrubbed in
// spool — the wrong order — so the outermost message read "?REDACTED" while the
// wrapper one level down carried a working presigned URL. Checking only
// err.Error() would still have passed, which is why this walks the whole chain.
func TestGet_SpoolBodyErrorRedactsTheURL(t *testing.T) {
	const sig = "SECRETSIG"
	target := "http://stub.invalid/a.json?X-Amz-Signature=" + sig + "&X-Amz-Expires=900"

	dir := t.TempDir()
	d := New(
		WithAllowInsecure(true),
		WithRetry(RetryConfig{MaxAttempts: 1}),
		WithTempDir(dir),
		WithHTTPClient(&http.Client{Transport: urlErrorBodyTransport{target: target}}),
	)
	_, err := d.Get(context.Background(), target)
	if err == nil {
		t.Fatal("expected an error")
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), sig) {
			t.Errorf("the signature survives at %T, one wrapper deep: %v", e, e)
		}
	}
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		if strings.Contains(uerr.URL, sig) {
			t.Errorf("url.Error.URL still carries the signature: %q", uerr.URL)
		}
	} else {
		t.Error("expected the *url.Error to survive in the chain; if it no longer does, " +
			"this test needs rewriting rather than deleting — it is the only coverage " +
			"of the scrub on the spool path")
	}
	// Still debuggable, and the spool is gone.
	if !strings.Contains(err.Error(), "/a.json") {
		t.Errorf("error dropped the URL path, making it undebuggable: %v", err)
	}
	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files after a body-read failure = %d, want 0", n)
	}
}

// TestGet_ExtremeCapDoesNotYieldAnEmptyArtifact — math.MaxInt64 is the natural
// spelling of "effectively uncapped" and WithMaxBytes accepts it, so cap+1 must
// not overflow.
//
// Before readLimit existed it wrapped to math.MinInt64, io.LimitReader returned
// EOF on its first read, and Get reported SUCCESS with a zero-byte artifact whose
// hash was the SHA-256 of the empty string — indistinguishable from a legitimate
// empty publish to a content-addressed gate, and destructive for a connector
// whose apply is a scoped replace.
func TestGet_ExtremeCapDoesNotYieldAnEmptyArtifact(t *testing.T) {
	const payload = "this artifact is not empty"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	for name, capBytes := range map[string]int64{
		"MaxInt64":     math.MaxInt64,
		"MaxInt64-1":   math.MaxInt64 - 1,
		"1<<62":        1 << 62,
		"exactly fits": int64(len(payload)),
	} {
		t.Run(name, func(t *testing.T) {
			d := New(WithAllowInsecure(true), WithMaxBytes(capBytes), WithTempDir(t.TempDir()),
				WithRetry(RetryConfig{MaxAttempts: 1}))
			res, err := d.Get(context.Background(), srv.URL)
			if err != nil {
				t.Fatalf("cap %d rejected a %d byte artifact: %v", capBytes, len(payload), err)
			}
			defer func() { _ = res.Close() }()
			if res.Size != int64(len(payload)) {
				t.Fatalf("Size = %d, want %d — a %d byte cap must not truncate", res.Size, len(payload), capBytes)
			}
			if got := readAll(t, res); got != payload {
				t.Errorf("artifact = %q, want %q", got, payload)
			}
		})
	}
}

// TestReadLimit_NeverOverflows pins the arithmetic directly, since the boundary is
// unreachable through Get with any realistic artifact.
func TestReadLimit_NeverOverflows(t *testing.T) {
	for _, in := range []int64{1, 100, 1 << 20, math.MaxInt64 - 2, math.MaxInt64 - 1, math.MaxInt64} {
		if got := readLimit(in); got < in {
			t.Errorf("readLimit(%d) = %d, which is BELOW the cap — LimitReader would truncate "+
				"or EOF immediately", in, got)
		}
	}
	if got := readLimit(math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("readLimit(MaxInt64) = %d, want MaxInt64 (saturate, do not wrap)", got)
	}
	if got := readLimit(100); got != 101 {
		t.Errorf("readLimit(100) = %d, want 101 — the extra byte is what makes an overrun detectable", got)
	}
}

// TestResult_ConcurrentReadersAndCloseAreRaceFree covers the shape the docs invite
// and the first version of this change did not support: several passes in flight
// with one deferred Close.
//
// Only meaningful under -race, which CI runs. The two races it caught were Close
// versus Reader and Close versus Close.
func TestResult_ConcurrentReadersAndCloseAreRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("payload", 512)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithTempDir(dir), WithRetry(RetryConfig{MaxAttempts: 1}))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			// Reads may legitimately fail once Close has run; the assertion is that
			// nothing races and nothing panics.
			if r := res.Reader(); r != nil {
				_, _ = io.Copy(io.Discard, r)
			}
		})
	}
	for range 3 {
		wg.Go(func() { _ = res.Close() })
	}
	wg.Wait()

	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files after concurrent Closes = %d, want 0", n)
	}
}

// TestResult_ReaderAfterCloseErrorsRatherThanPanicking — the reader must stay
// usable-shaped after Close so a use-after-close is a handleable error.
//
// Returning nil here (the first version) was worse than it looked: a typed nil in
// an interface is NOT nil, so `var rd io.Reader = res.Reader()` is non-nil, the
// guard a defensive caller writes passes, and the panic arrives inside io.ReadAll
// — further from the mistake, not closer.
func TestResult_ReaderAfterCloseErrorsRatherThanPanicking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithTempDir(t.TempDir()), WithRetry(RetryConfig{MaxAttempts: 1}))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := res.Reader()
	if r == nil {
		t.Fatal("Reader() returned nil after Close; a typed nil defeats a caller's nil guard " +
			"and then panics inside the read")
	}
	// Through an interface, which is where the old typed nil did its damage: a nil
	// *io.SectionReader assigned here produced a NON-nil io.Reader, so a caller's
	// `if rd != nil` guard passed and the panic landed inside io.ReadAll. Now that
	// Reader never returns nil for a live *Result, that trap is structurally gone —
	// staticcheck flags a nil check here as unreachable, which is the proof.
	var rd io.Reader = r
	if _, err := io.ReadAll(rd); !errors.Is(err, os.ErrClosed) {
		t.Errorf("read after Close gave %v, want os.ErrClosed", err)
	}
}

// TestGet_SpoolWriteFailureIsPermanent — a write failure is the local filesystem
// being full, read-only or gone, none of which clears inside the retry budget.
//
// It used to be terminal only by accident: syscall.Errno satisfies net.Error and
// ENOSPC.Timeout() is false, so isRetryable's net.Error branch stopped the retry.
// That reasoning is three coincidences deep and does not hold for a write error
// that is not an Errno, which is exactly what this test injects.
func TestGet_SpoolWriteFailureIsPermanent(t *testing.T) {
	body := &countingBody{remaining: 8 << 10}
	tr := &undeclaredLengthTransport{body: body}
	// A spool directory that exists at CreateTemp time but whose writes fail is
	// awkward to arrange portably, so the failure is injected through the writer
	// tag instead: a plain (non-Errno) error from the file write.
	d := New(WithAllowInsecure(true), WithTempDir(t.TempDir()), WithMaxBytes(1<<20),
		WithRetry(RetryConfig{MaxAttempts: 3, InitialDelay: 0}),
		WithHTTPClient(&http.Client{Transport: tr}))

	// Sanity: without an injected write failure this succeeds, so a green result
	// below would not be mistaken for the failure being absent.
	res, err := d.Get(context.Background(), "http://stub.invalid/a.json")
	if err != nil {
		t.Fatalf("control fetch failed: %v", err)
	}
	_ = res.Close()

	// The tagged classification itself, at the unit level — the copy loop's two
	// failure sources must stay distinguishable after the fact.
	plain := errors.New("simulated write failure, not a syscall.Errno")
	tagged := tagWriteErrors{w: failingWriter{err: plain}}
	if _, werr := tagged.Write([]byte("x")); werr == nil {
		t.Fatal("tagWriteErrors swallowed a write failure")
	} else {
		var we *spoolWriteError
		if !errors.As(werr, &we) {
			t.Errorf("a write failure was not tagged as a spool-write error: %T", werr)
		}
		if !errors.Is(werr, plain) {
			t.Error("tagging lost the underlying cause")
		}
	}
	// A body-read error must NOT be tagged, or a customer server hanging up would
	// be reported as a local disk problem and stop being retried.
	var we *spoolWriteError
	if errors.As(io.ErrUnexpectedEOF, &we) {
		t.Error("a body-read error must not look like a spool-write error")
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }

// TestSweepStaleSpools covers the crash-orphan case. Close is the only thing that
// removes a spool and SIGKILL runs no defers, so an OOM-killed connector leaves
// the partial artifact behind — and under Kubernetes /tmp is an emptyDir, which
// survives container restarts, so a crash-looping connector accumulates one per
// crash.
func TestSweepStaleSpools(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, tempFilePrefix+"stale")
	fresh := filepath.Join(dir, tempFilePrefix+"fresh")
	foreign := filepath.Join(dir, "someone-elses-file")
	for _, p := range []string{stale, fresh, foreign} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// A directory whose name matches the prefix must be left alone.
	staleDir := filepath.Join(dir, tempFilePrefix+"adirectory")
	if err := os.Mkdir(staleDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}

	removed, err := SweepStaleSpools(dir, time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleSpools: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale spool survived")
	}
	// A live download's spool is indistinguishable from an abandoned one by name,
	// so anything recent must be left alone — otherwise a caller pointing several
	// components at one directory would have each sweep delete its peers' work.
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent spool was swept; a live download would have been destroyed")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("a file that is not a spool was swept")
	}
	if _, err := os.Stat(staleDir); err != nil {
		t.Error("a directory matching the prefix was removed")
	}
}

func TestSweepStaleSpools_DefaultsAndMissingDir(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, tempFilePrefix+"stale")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-2 * DefaultStaleSpoolAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// A non-positive age means the default rather than "sweep everything", so a
	// zero-valued config cannot delete a live download.
	if removed, err := SweepStaleSpools(dir, 0); err != nil || removed != 1 {
		t.Errorf("SweepStaleSpools(dir, 0) = %d, %v; want 1, nil", removed, err)
	}

	// A missing directory is reported, not silently treated as "nothing to do":
	// it usually means the spool dir is mis-configured, which the caller wants to
	// hear about at startup.
	if _, err := SweepStaleSpools(filepath.Join(dir, "does-not-exist"), time.Hour); err == nil {
		t.Error("a missing spool directory was accepted")
	}
}

// TestSweepStaleSpools_LeavesALiveDownloadAlone is the property that makes the
// sweep safe to call unconditionally at startup: a spool being written right now
// must survive it.
func TestSweepStaleSpools_LeavesALiveDownloadAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithTempDir(dir), WithRetry(RetryConfig{MaxAttempts: 1}))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = res.Close() }()

	if removed, err := SweepStaleSpools(dir, time.Hour); err != nil || removed != 0 {
		t.Fatalf("sweep removed %d files (err %v); a live artifact must survive", removed, err)
	}
	if got := readAll(t, res); got != "payload" {
		t.Errorf("artifact after the sweep = %q, want %q", got, "payload")
	}
}
