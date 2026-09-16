package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This file holds the SJ24K-53 spooling tests: the artifact goes to a temp file
// rather than a byte slice, the digest falls out of the transfer, and the file is
// deleted on every path.
//
// The cap tests here drive a STUB TRANSPORT rather than httptest, and that is
// deliberate. Two facts about net/http make a server-based cap test prove the
// wrong thing:
//
//   - For a small response the server computes and sends Content-Length, so the
//     advisory pre-check rejects the artifact and the during-copy check — the
//     authoritative one — never runs. A test written that way stays green if the
//     copy check is deleted outright.
//   - Counting bytes the HANDLER wrote measures the socket buffer, not the
//     reader: the server can run several chunks ahead of a client that has
//     already stopped. Counting bytes the CLIENT pulled from the body is exactly
//     the quantity the "abandon at the cap" property is about, and has no
//     buffering noise in it.

// countingBody serves n bytes of filler, recording how many were actually read.
type countingBody struct {
	remaining int64
	mu        sync.Mutex
	read      int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > b.remaining {
		n = b.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func (b *countingBody) Close() error { return nil }

func (b *countingBody) bytesRead() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read
}

// undeclaredLengthTransport answers every request with a 200 whose length is
// UNDECLARED (ContentLength -1), the shape a chunked response has on the client
// side.
type undeclaredLengthTransport struct {
	body    io.ReadCloser
	header  http.Header
	mu      sync.Mutex
	Attempt int
}

func (t *undeclaredLengthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.Attempt++
	t.mu.Unlock()
	h := t.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        h,
		Body:          t.body,
		ContentLength: -1,
	}, nil
}

func (t *undeclaredLengthTransport) attempts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Attempt
}

// spoolFiles lists the artifact spool files currently in dir. Every leak
// assertion in this package is a count of these, because "the temp file is
// deleted" is not observable from a Result once it is closed.
func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, tempFilePattern))
	if err != nil {
		t.Fatalf("glob spool dir: %v", err)
	}
	return matches
}

func spoolCount(t *testing.T, dir string) int {
	t.Helper()
	return len(spoolFiles(t, dir))
}

// readAll drains a Result through a fresh reader, which is how every consumer
// gets at the bytes now that there is no Body slice.
func readAll(t *testing.T, res *Result) string {
	t.Helper()
	b, err := io.ReadAll(res.Reader())
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	return string(b)
}

// TestGet_HashMatchesAnIndependentSHA256 is the acceptance criterion for hashing
// during the transfer: the streamed digest must equal one taken over the artifact
// separately. A mis-wired MultiWriter — hashing the buffer rather than the bytes
// written, or a hasher re-created per chunk — shows up here and nowhere else,
// since every other test only cares that SOME hash came back.
//
// The sizes straddle the copy buffer on purpose. A single-chunk payload cannot
// catch a per-chunk hasher at all.
func TestGet_HashMatchesAnIndependentSHA256(t *testing.T) {
	for _, size := range []int{0, 1, copyBufferSize - 1, copyBufferSize, copyBufferSize + 1, 3*copyBufferSize + 17} {
		t.Run(fmt.Sprintf("%d_bytes", size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i % 251) // varied, so a chunk-boundary bug shifts the digest
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			defer srv.Close()

			d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithTempDir(t.TempDir()),
				WithMaxBytes(int64(size)+1))
			res, err := d.Get(context.Background(), srv.URL)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = res.Close() }()

			want := sha256.Sum256(payload)
			if res.Hash != hex.EncodeToString(want[:]) {
				t.Errorf("Hash = %q, want %q", res.Hash, hex.EncodeToString(want[:]))
			}
			// Bare lower-case hex, no algorithm prefix: a publisher's claimed
			// checksum is normalized to exactly this form, so a prefix or upper
			// case here would fail every comparison.
			if len(res.Hash) != sha256.Size*2 || strings.ContainsAny(res.Hash, "ABCDEF:") {
				t.Errorf("Hash = %q, want bare lower-case hex", res.Hash)
			}
			if res.Size != int64(size) {
				t.Errorf("Size = %d, want %d", res.Size, size)
			}
			// And the spooled bytes themselves must be the artifact, not just
			// something with the right digest.
			if got := readAll(t, res); !bytes.Equal([]byte(got), payload) {
				t.Errorf("spooled %d bytes, want the %d byte artifact", len(got), size)
			}
		})
	}
}

// TestResult_ReaderIsIndependentPerCall covers the property the sj24k asset
// export depends on: it reads the artifact three times (embedded class
// catalogue, then vertices, then edges). With one shared cursor the second pass
// would read nothing — a silent empty ingest rather than an error.
func TestResult_ReaderIsIndependentPerCall(t *testing.T) {
	const payload = "line-one\nline-two\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithTempDir(t.TempDir()))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = res.Close() }()

	for pass := range 3 {
		if got := readAll(t, res); got != payload {
			t.Fatalf("pass %d read %q, want %q — each Reader must start at the beginning", pass, got, payload)
		}
	}

	// Two readers live at once must not disturb each other's offset, which is
	// what makes the passes safe to interleave rather than merely repeat.
	a, b := res.Reader(), res.Reader()
	head := make([]byte, 4)
	if _, err := io.ReadFull(a, head); err != nil {
		t.Fatalf("read from first reader: %v", err)
	}
	rest, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("read from second reader: %v", err)
	}
	if string(rest) != payload {
		t.Errorf("second reader saw %q, want the whole artifact %q", rest, payload)
	}
}

// TestResult_ReaderIsSeekable pins what an upload retry needs: replaying the body
// means seeking back to the start.
func TestResult_ReaderIsSeekable(t *testing.T) {
	const payload = "abcdefghij"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithTempDir(t.TempDir()))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = res.Close() }()

	r := res.Reader()
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	again, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(again) != payload {
		t.Errorf("after rewind read %q, want %q", again, payload)
	}
	if r.Size() != int64(len(payload)) {
		t.Errorf("Reader().Size() = %d, want %d", r.Size(), len(payload))
	}
}

// TestResult_CloseDeletesTheSpoolFile — the success-path half of
// delete-on-every-path.
func TestResult_CloseDeletesTheSpoolFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithTempDir(dir))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := spoolCount(t, dir); n != 1 {
		t.Fatalf("spool files while the result is open = %d, want 1", n)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files after Close = %d, want 0", n)
	}
	// Idempotent, so a deferred Close alongside an explicit one is safe. A second
	// Close that reported an error would make `defer res.Close()` a liability.
	if err := res.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Reader stays non-nil after Close, and reads fail instead. See
	// TestResult_ReaderAfterCloseErrorsRatherThanPanicking for why: a typed nil in
	// an interface is not nil, so returning nil here defeated a caller's guard and
	// then panicked further from the mistake.
	if res.Reader() == nil {
		t.Error("Reader() after Close must not be nil — a typed nil defeats a nil guard")
	}
}

// TestGet_DuringCopyCapIsAuthoritative — an artifact whose length is never
// declared must still be capped, because the Content-Length pre-check is
// advisory: a chunked source declares nothing and a hostile one can understate
// it. This is the check that must not be removable without a failing test.
func TestGet_DuringCopyCapIsAuthoritative(t *testing.T) {
	body := &countingBody{remaining: 5000}
	tr := &undeclaredLengthTransport{body: body}
	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithMaxBytes(100), WithTempDir(dir),
		WithRetry(RetryConfig{MaxAttempts: 3, InitialDelay: 0}),
		WithHTTPClient(&http.Client{Transport: tr}))

	_, err := d.Get(context.Background(), "http://stub.invalid/a.json")
	if err == nil {
		t.Fatal("an over-cap body with no declared length was accepted")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error does not mention the cap: %v", err)
	}
	if got := tr.attempts(); got != 1 {
		t.Errorf("attempts = %d, want 1 — an over-cap body is permanent and must not be retried", got)
	}
	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files after an over-cap rejection = %d, want 0", n)
	}
}

// TestGet_OverCapStopsAtTheCapNotTheFullLength bounds the wasted work: reading
// the whole body and checking afterwards would reject correctly too, so only a
// byte count separates the two implementations. Under a 64 KiB cap a 4 MiB
// artifact must cost 64 KiB + 1, not 4 MiB.
func TestGet_OverCapStopsAtTheCapNotTheFullLength(t *testing.T) {
	const capBytes = 64 << 10
	const total = 4 << 20
	body := &countingBody{remaining: total}
	tr := &undeclaredLengthTransport{body: body}
	d := New(WithAllowInsecure(true), WithMaxBytes(capBytes), WithTempDir(t.TempDir()),
		WithRetry(RetryConfig{MaxAttempts: 1}),
		WithHTTPClient(&http.Client{Transport: tr}))

	if _, err := d.Get(context.Background(), "http://stub.invalid/a.json"); err == nil {
		t.Fatal("an over-cap body was accepted")
	}
	// Exact: the LimitReader hands out cap+1 and not one byte more, and the
	// counting body is read directly with no intervening buffering.
	if got := body.bytesRead(); got != capBytes+1 {
		t.Errorf("read %d bytes for a %d byte cap, want exactly %d (cap+1)", got, capBytes, capBytes+1)
	}
}

// TestGet_RejectsOverCapContentLengthWithoutSpooling covers the advisory
// pre-check: a well-behaved source that declares an over-cap length is refused
// before a spool file is created at all.
func TestGet_RejectsOverCapContentLengthWithoutSpooling(t *testing.T) {
	body := &countingBody{remaining: 1000}
	tr := &declaredLengthTransport{body: body, length: 1000}
	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithMaxBytes(10), WithTempDir(dir),
		WithRetry(fastRetry), WithHTTPClient(&http.Client{Transport: tr}))

	_, err := d.Get(context.Background(), "http://stub.invalid/a.json")
	if err == nil {
		t.Fatal("an artifact declaring an over-cap length was accepted")
	}
	if !strings.Contains(err.Error(), "declares") {
		t.Errorf("error does not name the declared length: %v", err)
	}
	if got := body.bytesRead(); got != 0 {
		t.Errorf("read %d bytes, want 0 — a declared over-cap length must be refused before transfer", got)
	}
	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files = %d, want 0 — no file should be created at all", n)
	}
}

// declaredLengthTransport answers with a 200 that DECLARES its length, so the
// advisory pre-check has something to act on.
type declaredLengthTransport struct {
	body   io.ReadCloser
	length int64
}

func (t *declaredLengthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          t.body,
		ContentLength: t.length,
	}, nil
}

// TestGet_UnderstatedContentLengthIsStillCapped — the pre-check must not be
// load-bearing. A source that declares a small length and then sends far more
// has to be caught by the copy, or one header would bypass the cap entirely.
func TestGet_UnderstatedContentLengthIsStillCapped(t *testing.T) {
	body := &countingBody{remaining: 5000}
	tr := &declaredLengthTransport{body: body, length: 10} // the lie
	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithMaxBytes(100), WithTempDir(dir),
		WithRetry(RetryConfig{MaxAttempts: 1}), WithHTTPClient(&http.Client{Transport: tr}))

	if _, err := d.Get(context.Background(), "http://stub.invalid/a.json"); err == nil {
		t.Fatal("an understated Content-Length let an over-cap body through")
	}
	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files = %d, want 0", n)
	}
}

// TestGet_MidTransferFailureDeletesTheSpool is the third delete-on-every-path
// case and the one most likely to regress: the failure happens after the file
// exists and is partly written, so the cleanup cannot ride on a success-path
// defer.
//
// Driven through a real server that announces a length and then drops the
// connection, which is what a customer endpoint or object store dying mid-download
// actually looks like to net/http — a body read that ends in an unexpected EOF.
func TestGet_MidTransferFailureDeletesTheSpool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 512))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithMaxBytes(1<<20), WithRetry(RetryConfig{MaxAttempts: 1}),
		WithTempDir(dir))
	_, err := d.Get(context.Background(), srv.URL+"/a.json?X-Amz-Signature=SECRETSIG")
	if err == nil {
		t.Fatal("a truncated transfer was reported as success")
	}
	if n := spoolCount(t, dir); n != 0 {
		t.Errorf("spool files after a mid-transfer failure = %d, want 0 (left: %v)", n, spoolFiles(t, dir))
	}
	// ⚠️ This loop is NOT the redaction coverage, and must not be mistaken for it.
	// A hijacked transfer surfaces as a bare io.ErrUnexpectedEOF — no *url.Error,
	// no URL anywhere in the chain — so it cannot fail whatever the scrub does,
	// which is exactly how an ordering defect on this path once shipped with a
	// test claiming to cover it. Retained only as a cheap belt-and-braces check.
	// The real coverage is TestGet_SpoolBodyErrorRedactsTheURL, which injects a
	// *url.Error from the body read.
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), "SECRETSIG") {
			t.Errorf("error chain leaked the URL signature at %T: %v", e, e)
		}
	}
}

// TestGet_RetriedAttemptsLeaveNoSpoolBehind — each attempt spools into its own
// file, so a retried success must leave exactly one. Three attempts each leaking
// 300 MB is the outage this guards against.
//
// ⚠️ The failing attempts MUST fail PART WAY THROUGH THE BODY, not with a status
// code. An earlier version used 502s, which return at the status check before
// spool() is ever called — so no failed attempt created a file and the final
// count of 1 could not distinguish a leaking retry loop from a clean one. Probing
// it confirmed the spool count at the start of each attempt was [0 0 0]. A
// truncated transfer is the only shape that actually spools and then fails.
func TestGet_RetriedAttemptsLeaveNoSpoolBehind(t *testing.T) {
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		if attempt < 3 {
			// Announce more than will be sent, write some of it — so the spool
			// file exists and is partly written — then drop the connection.
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, 4096))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		_, _ = w.Write([]byte("recovered"))
		_ = r
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithTempDir(dir))
	res, err := d.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := spoolCount(t, dir); got != 1 {
		t.Errorf("spool files after a retried success = %d, want exactly 1 (left: %v)", got, spoolFiles(t, dir))
	}
	if got := readAll(t, res); got != "recovered" {
		t.Errorf("artifact = %q", got)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := spoolCount(t, dir); got != 0 {
		t.Errorf("spool files after Close = %d, want 0", got)
	}
}

// TestGet_UnwritableTempDirIsPermanent — a mis-configured spool directory is a
// deployment fault: it will not fix itself, so retrying only delays the report.
// This is also the failure a read-only container filesystem produces when no
// writable temp dir was mounted, which is worth failing loudly and once.
func TestGet_UnwritableTempDirIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	var attempts int
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		_, _ = w.Write([]byte("payload"))
	}))
	defer countingSrv.Close()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	d := New(WithAllowInsecure(true), WithRetry(fastRetry), WithTempDir(missing))
	_, err := d.Get(context.Background(), countingSrv.URL)
	if err == nil {
		t.Fatal("a missing spool directory was accepted")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the spool directory, so an operator cannot fix it: %v", err)
	}
	if !strings.Contains(err.Error(), "spool") {
		t.Errorf("error does not say what failed: %v", err)
	}
	// The terminal behaviour, which the message alone cannot show: without it this
	// test asserted only wording and would have passed with three attempts.
	//
	// ⚠️ It does NOT isolate the Permanent wrapper, and it is worth knowing why
	// rather than assuming it does. os.CreateTemp on a missing directory returns a
	// *fs.PathError wrapping syscall.ENOENT, syscall.Errno satisfies net.Error, and
	// ENOENT.Timeout() is false — so isRetryable stops the retry on its own
	// (measured: isRetryable reports false for the raw error). One attempt
	// therefore has two independent causes and removing the marker keeps this
	// green. The marker stays anyway, because depending on that coincidence is the
	// fragility spoolWriteError exists to remove: it does not hold for an error
	// that is not an Errno, and it would break silently if isRetryable's net.Error
	// branch were ever tightened.
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a mis-configured spool directory will not "+
			"fix itself, so retrying only delays the report", attempts)
	}
	_ = srv
}

// TestResult_NilIsUsable — a caller that got an error has no Result, and
// `res, err := Get(...); defer res.Close()` with the defer above the error check
// must not panic.
func TestResult_NilIsUsable(t *testing.T) {
	var res *Result
	if err := res.Close(); err != nil {
		t.Errorf("Close on nil Result: %v", err)
	}
	if res.Reader() != nil {
		t.Error("Reader on nil Result must be nil")
	}
}

// TestWithTempDir_EmptyIsIgnored — an unset config value must leave the default
// in place, so a kit reading an optional env var does not accidentally spool
// somewhere unintended.
func TestWithTempDir_EmptyIsIgnored(t *testing.T) {
	dir := t.TempDir()
	d := New(WithTempDir(dir), WithTempDir("   "))
	if d.tempDir != dir {
		t.Errorf("tempDir = %q, want the earlier non-empty value %q", d.tempDir, dir)
	}
	if got := New().spoolDir(); got == "" {
		t.Error("default spoolDir is empty; it must name os.TempDir so errors are actionable")
	}
}
