// Package fetch downloads an artifact a customer system published, with the
// containment guards every kit needs and none of the domain knowledge only one
// kit has.
//
// It exists because the "upstream publishes, we fetch" shape is generic. A
// customer system announces new content — typically by webhook, carrying a
// presigned or otherwise time-limited URL — and the CONNECTOR dereferences that
// URL, never the platform. That split is deliberate SSRF containment: the URL is
// customer-supplied, and the platform core must not be the thing that follows it.
// A connector performing that fetch needs, every time:
//
//   - HTTPS only, so a downgraded or plaintext URL cannot be honoured silently
//   - a size cap, so a hostile or mis-configured source cannot exhaust the host
//   - an optional host allowlist, so a leaked webhook credential cannot redirect
//     the fetch at an arbitrary origin
//   - a credential scoped to ONE host, so a poll token is not leaked to a
//     different-host presigned URL that authenticates itself via query params
//   - bounded timeout and retry, distinguishing "will not succeed" from
//     "might succeed later"
//
// The SDK supplies the MECHANISM. Policy stays with the kit: it decides the cap,
// whether to enforce an allowlist and which hosts belong on it (usually from
// per-tenant deployment configuration, since a customer's bucket host differs per
// tenant), and which host may see a credential.
//
// # The artifact is spooled to a temp file, not buffered in memory
//
// Get writes the body straight to a temp file and hands back a Result the caller
// reads from and MUST Close (SJ24K-53). It used to return the whole body as a
// []byte, which made peak memory scale with the artifact: a 210 MB customer
// export cost 210 MB of heap per connector, per cycle, for every tenant sharing a
// node — and every consumer that needed a second look at the bytes (a content
// hash, a checksum comparison, a second parse pass) either kept that slice alive
// or was tempted to make its own copy.
//
// Because the transfer is the only pass over the bytes that is unavoidable, the
// SHA-256 is computed DURING it, through an io.MultiWriter. So the digest a
// caller needs for a no-op gate or a publisher-checksum comparison falls out of
// the download for free, rather than costing a second full read.
//
// Two consequences worth knowing before using this package:
//
//   - Result owns an open file descriptor and a file on disk. Close deletes it.
//     A caller that forgets leaks both, and at these sizes a leak per failed
//     cycle is its own outage.
//   - Result.Reader hands out independent readers over the same spooled bytes,
//     so a caller may make as many passes as it likes (the sj24k asset export
//     takes three) without re-downloading and without a shared cursor to rewind.
//
// # Where the temp file lands, and why that is not always "off memory"
//
// The spool directory defaults to os.TempDir and is overridable with
// WithTempDir. That option exists for a specific, measured reason: writing to a
// RAM-backed filesystem moves the artifact off the Go heap but NOT out of memory.
//
// The ONTOGIS sidecar runtimes differ here, and the difference is invisible in
// the code that spools. Under Kubernetes /tmp is an emptyDir with the default
// medium, i.e. node disk, so a spooled artifact genuinely leaves the container's
// memory allowance. Under Docker the platform mounts /tmp as tmpfs, whose pages
// are charged to the container's memory cgroup in full — measured on Docker
// 29.4.0: a 300 MB write to a tmpfs /tmp in a 512 MiB container moved
// memory.current from 2.9 MB to 317 MB, with memory.stat shmem accounting for
// every byte. So on a Docker-run sidecar the spool must either fit the memory
// limit or be pointed at a disk-backed mount with WithTempDir.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultMaxBytes caps a downloaded artifact at 64 MiB.
//
// It is deliberately conservative rather than generous, and it stays at 64 MiB
// even though the artifact is now spooled to disk (SJ24K-53). Two reasons:
//
//   - It matches the platform ontology-snapshot intake's own body cap, so a
//     fetch that succeeds under the default is not rejected downstream for size.
//     A connector cap ABOVE the intake's would defer a clear, immediate
//     rejection into a 413 that only arrives after a full download.
//   - A cap is a policy decision belonging to the kit, which knows what its
//     upstream publishes. An SDK default that silently accommodated the largest
//     artifact anyone might send would remove the guard for every kit that never
//     thought about it.
//
// A connector whose upstream publishes a DATA export rather than a catalogue
// should raise this explicitly with WithMaxBytes, and size it against the
// sidecar's memory limit when the spool directory is RAM-backed (see the package
// doc).
const DefaultMaxBytes int64 = 64 << 20

// DefaultTimeout bounds a single attempt end-to-end.
const DefaultTimeout = 2 * time.Minute

// copyBufferSize is the transfer buffer, and it is what "peak memory is
// proportional to the copy buffer, not the artifact" refers to. 256 KiB rather
// than io.Copy's internal 32 KiB: at 300 MB that is 1,200 write syscalls instead
// of 9,600, and 256 KiB is still negligible beside any sidecar's memory limit.
const copyBufferSize = 256 << 10

// tempFilePattern names the spooled file. The prefix is searchable, so an
// operator finding one of these left behind knows which component to blame.
const tempFilePattern = "oga-fetch-artifact-*"

// Result is a downloaded artifact, spooled to a temp file.
//
// The caller OWNS it and MUST Close it — that is what deletes the temp file and
// releases the descriptor. Get returns a Result only on success; on every
// failure path Get cleans up its own partial spool, so a caller that got an
// error has nothing to close.
type Result struct {
	// Hash is the SHA-256 of the artifact as bare lower-case hex, with no
	// algorithm prefix, computed during the transfer.
	//
	// This is the digest of the RAW bytes as published, which is the only form
	// comparable with a publisher's asserted checksum (computed over the same
	// file) and the conservative key for a no-op gate: a semantically identical
	// re-publish differing only in key order re-ingests, where hashing a
	// normalized form risks calling two genuinely different artifacts equal.
	Hash string

	// Size is the artifact's length in bytes, and always equals the spooled
	// file's length.
	Size int64

	// ETag is the source's entity tag with any surrounding quotes stripped, so a
	// caller can compare it directly. Empty when the source sent none.
	//
	// It is an opaque cache validator, NOT a digest — an S3 multipart ETag is
	// neither a SHA-256 nor a whole-object MD5 — so it must never be used where a
	// checksum is meant. Use Hash for that.
	ETag string

	// file is the spooled artifact, held open so Reader can hand out
	// independent io.ReaderAt-backed views of it.
	file *os.File
	// path is retained for the unlink in Close: it is the name Close removes,
	// not something a caller should reach for.
	path string
}

// Reader returns an independent reader over the whole artifact, positioned at
// the start.
//
// Independent is the point. A caller that needs several passes — the sj24k asset
// export is read once for its embedded class catalogue, once for vertices and
// once for edges — gets a fresh cursor each time rather than having to rewind a
// shared one, and forgetting to rewind is the bug this shape makes unwritable.
// Concurrent readers are safe for the same reason: io.SectionReader keeps its
// offset itself and reads through ReadAt, which does not move the file's own
// offset.
//
// The returned reader is also an io.ReadSeeker, which is what lets a caller that
// retries an upload replay the body.
//
// After Close the reader is dead: reads fail on a closed descriptor. Reader
// returns nil once Close has run, so a use-after-close is a nil dereference at
// the call site rather than a confusing read error further away.
func (r *Result) Reader() *io.SectionReader {
	if r == nil || r.file == nil {
		return nil
	}
	return io.NewSectionReader(r.file, 0, r.Size)
}

// Close releases the descriptor and deletes the temp file. It is safe to call
// more than once, so a `defer res.Close()` costs nothing next to an explicit
// close on a success path.
//
// The unlink runs even when the close fails: leaving a multi-hundred-megabyte
// file behind because a descriptor misbehaved would be the worse of the two
// outcomes, and on Unix removing a name whose descriptor is still open is
// legitimate anyway.
func (r *Result) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	f, path := r.file, r.path
	r.file, r.path = nil, ""
	closeErr := f.Close()
	rmErr := os.Remove(path)
	if closeErr != nil {
		return closeErr
	}
	if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return rmErr
	}
	return nil
}

// Downloader fetches artifacts under the guards described on the package.
// Construct with New; the zero value is not usable.
type Downloader struct {
	http          *http.Client
	maxBytes      int64
	tempDir       string
	retry         RetryConfig
	allowInsecure bool
	authToken     string
	authHost      string
	hostAllowlist map[string]struct{}
}

// Option configures a Downloader.
type Option func(*Downloader)

// WithHTTPClient overrides the HTTP client (a test injects a transport; a kit
// might supply one with a custom proxy or TLS config).
func WithHTTPClient(c *http.Client) Option {
	return func(d *Downloader) {
		if c != nil {
			d.http = c
		}
	}
}

// WithMaxBytes overrides the size cap. Non-positive values are ignored, so a
// zero-valued config cannot accidentally remove the cap.
func WithMaxBytes(n int64) Option {
	return func(d *Downloader) {
		if n > 0 {
			d.maxBytes = n
		}
	}
}

// WithTempDir overrides the directory the artifact is spooled to. An empty value
// is ignored, leaving os.TempDir.
//
// Set it when the default temp directory is RAM-backed and the artifact is large
// enough for that to matter: spooling to a tmpfs moves the bytes off the Go heap
// but leaves them charged to the container's memory limit, which defeats the
// point. See the package doc for the measured Docker-versus-Kubernetes
// difference.
//
// The directory must exist and be writable; Get reports a permanent error if it
// is not, since a mis-configured spool directory will not fix itself on a retry.
func WithTempDir(dir string) Option {
	return func(d *Downloader) {
		if s := strings.TrimSpace(dir); s != "" {
			d.tempDir = s
		}
	}
}

// WithRetry overrides the retry policy.
func WithRetry(rc RetryConfig) Option {
	return func(d *Downloader) { d.retry = rc }
}

// WithAllowInsecure permits http:// URLs. Off by default. Enable only for tests
// or an explicitly trusted in-cluster source — never for a customer-supplied URL,
// which is the case this package exists to contain.
func WithAllowInsecure(allow bool) Option {
	return func(d *Downloader) { d.allowInsecure = allow }
}

// WithBearer attaches token as an Authorization: Bearer header ONLY on requests
// whose host matches host.
//
// The scoping is the point, not a convenience. A poll source is authenticated by
// a long-lived credential; a webhook-delivered presigned URL authenticates itself
// through signed query parameters and belongs to a different origin (an object
// store). Sending the poll credential to that origin would leak it for no benefit.
// The comparison ignores the port, so a source URL carrying an explicit port still
// receives its credential.
func WithBearer(token, host string) Option {
	return func(d *Downloader) {
		d.authToken = strings.TrimSpace(token)
		d.authHost = normalizeHost(host)
	}
}

// WithHostAllowlist restricts downloads to the given hosts (case-insensitive,
// port-ignored). Empty entries are skipped, and an allowlist that reduces to
// nothing is NOT enforced — an empty set means "unset", never "deny everything",
// so a kit reading the list from optional configuration degrades to unrestricted
// rather than to a connector that can fetch nothing.
//
// Recommended whenever a fetch target arrives in a webhook body: even a validly
// signed notification can then only point the connector at a known origin.
func WithHostAllowlist(hosts []string) Option {
	return func(d *Downloader) {
		set := make(map[string]struct{}, len(hosts))
		for _, h := range hosts {
			if n := normalizeHost(h); n != "" {
				set[n] = struct{}{}
			}
		}
		if len(set) == 0 {
			d.hostAllowlist = nil
			return
		}
		d.hostAllowlist = set
	}
}

// normalizeHost lowercases and trims a host, dropping any port so every
// comparison in this package is port-agnostic and consistent. Without the single
// helper the allowlist (which compares url.Hostname) and the credential scope
// (which is easy to write against url.Host) disagree the moment a URL carries a
// port — the credential then silently never attaches.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil && host != "" {
		return host
	}
	return h
}

// New builds a Downloader with the documented defaults: HTTPS only, 64 MiB cap,
// 2 minute per-attempt timeout, bounded retry, os.TempDir for the spool, no
// allowlist, no credential.
func New(opts ...Option) *Downloader {
	d := &Downloader{
		http:     &http.Client{Timeout: DefaultTimeout},
		maxBytes: DefaultMaxBytes,
		retry:    DefaultRetry,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// safeURL renders u for an error message or a log with every credential-bearing
// part removed.
//
// url.URL.Redacted is NOT sufficient on its own: it replaces only the userinfo
// PASSWORD and leaves the query string intact. For the presigned URLs this
// package is built to fetch, the query string IS the credential — an AWS
// X-Amz-Signature, an Azure SAS token — so relying on Redacted alone would put a
// working signed URL into every error string and, from there, into logs.
func safeURL(u *url.URL) string {
	c := *u
	c.Fragment = ""
	if c.RawQuery != "" {
		// Note that a query existed, without revealing any of it: "no query" and
		// "query removed" are different facts when debugging a signed URL.
		c.RawQuery = "REDACTED"
	}
	return c.Redacted()
}

// Get fetches rawURL under the configured guards, spooling the body to a temp
// file and hashing it in the same pass.
//
// On success the caller owns the returned Result and MUST Close it. On failure
// nothing is returned and nothing is left on disk: every path that abandons a
// spool — a rejected URL, an over-cap body, a mid-transfer transport failure, an
// attempt that will be retried — deletes its own partial file before returning.
//
// Scheme, host and allowlist are validated BEFORE any request is made, and each
// rejection is permanent: a URL that is not allowed will not become allowed on a
// retry, so retrying would only delay the error. An over-cap body is permanent
// for the same reason — it will not shrink. A network error or a 5xx is
// retryable; a 4xx is not.
//
// The returned error never contains the URL's query string or userinfo password,
// since for a presigned URL those are the credential. That holds for the whole
// error chain, not just the outermost message: safeURL covers the message this
// function assembles, and scrubURLError covers the *url.Error that net/http
// produces underneath it, whose own Error() would otherwise print the URL
// verbatim.
func (d *Downloader) Get(ctx context.Context, rawURL string) (*Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, Permanent(fmt.Errorf("invalid download url: %w", err))
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !d.allowInsecure {
			return nil, Permanent(fmt.Errorf("download url must be https (got %q)", u.Scheme))
		}
	default:
		return nil, Permanent(fmt.Errorf("unsupported download scheme %q (https only)", u.Scheme))
	}
	if u.Host == "" {
		return nil, Permanent(errors.New("download url has no host"))
	}
	if len(d.hostAllowlist) > 0 {
		if _, ok := d.hostAllowlist[normalizeHost(u.Hostname())]; !ok {
			return nil, Permanent(fmt.Errorf("download host %q is not in the allowlist", u.Hostname()))
		}
	}

	safe := safeURL(u)

	var res *Result
	err = Do(ctx, d.retry, func() error {
		// A retried attempt must not inherit the previous one's spool. attempt
		// cleans up its own failures, so this only guards the case where a
		// successful attempt is somehow followed by another — cheap insurance
		// against a future edit to the retry loop leaking a 300 MB file.
		if res != nil {
			_ = res.Close()
			res = nil
		}
		r, ferr := d.attempt(ctx, u.String(), safe)
		if ferr != nil {
			return ferr
		}
		res = r
		return nil
	})
	if err != nil {
		// Do returns an error only when no attempt succeeded, but closing here
		// keeps "an error means nothing to clean up" true by construction rather
		// than by reading the retry loop.
		if res != nil {
			_ = res.Close()
		}
		return nil, fmt.Errorf("download %s: %w", safe, err)
	}
	return res, nil
}

// scrubURLError removes the credential-bearing URL that net/http puts inside a
// *url.Error.
//
// url.Error.Error() renders its URL field verbatim, query string and all, and
// Client.Do / NewRequest return exactly that type — so an unmodified network
// error carries a WORKING presigned URL, which is precisely what safeURL exists
// to prevent. safeURL on the surrounding message is not enough on its own.
//
// Two details make this correct, and both are load-bearing:
//
//   - It MUST run before any fmt.Errorf wraps the error. fmt.Errorf renders %w
//     into a stored string at construction time, so once the raw URL has been
//     folded into an outer message, rewriting the inner *url.Error changes
//     nothing. That is why this is applied at the point the error is produced
//     rather than at the boundary where the message is assembled.
//   - It rewrites the URL IN PLACE rather than rebuilding the chain. errors.AsType
//     yields a pointer to the actual *url.Error, so every enclosing wrapper — the
//     permanent marker, the attempt-count wrapper — keeps referring to the same
//     value, and retry classification is untouched: url.Error still satisfies
//     net.Error and still forwards Timeout() to its cause.
//
// Mutation is safe because net/http constructs the *url.Error for this call and
// hands it to no one else.
func scrubURLError(err error, safe string) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		uerr.URL = safe
	}
	return err
}

func (d *Downloader) attempt(ctx context.Context, target, safe string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, Permanent(scrubURLError(err, safe))
	}
	if d.authToken != "" && d.authHost != "" && normalizeHost(req.URL.Host) == d.authHost {
		req.Header.Set("Authorization", "Bearer "+d.authToken)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, scrubURLError(err, safe) // network error — retryable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, StatusError(resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	// A declared length over the cap is refused before a single byte is
	// transferred. Advisory only — Content-Length is absent on a chunked response
	// and a hostile source can understate it — so the during-copy check below
	// stays authoritative. It is worth having because the common case of a
	// genuinely-too-large artifact is a well-behaved source that declares its
	// size, and there is no reason to spend the bandwidth and the disk to
	// rediscover what it already told us.
	if resp.ContentLength > d.maxBytes {
		return nil, Permanent(fmt.Errorf("download declares %d bytes, over the %d byte cap",
			resp.ContentLength, d.maxBytes))
	}

	return d.spool(resp, safe)
}

// spool streams the response body to a temp file, hashing as it goes, and
// enforces the cap during the copy.
//
// Every failure after the file exists deletes it before returning — that is the
// whole reason this is a function rather than inline in attempt: one place owns
// the cleanup, so a new early return cannot forget it.
func (d *Downloader) spool(resp *http.Response, safe string) (*Result, error) {
	f, err := os.CreateTemp(d.tempDir, tempFilePattern)
	if err != nil {
		// A missing, unwritable or read-only spool directory is a deployment
		// fault, not a transient one: retrying cannot create the directory, and
		// the error text names the path so an operator can fix it. This is the
		// failure a read-only container root filesystem produces when no writable
		// temp dir was mounted.
		return nil, Permanent(fmt.Errorf("create spool file in %q: %w", d.spoolDir(), err))
	}

	res, err := writeSpool(f, resp.Body, d.maxBytes)
	if err != nil {
		discardSpool(f)
		// Scrubbed before the wrap, for the reason given on scrubURLError: a
		// mid-body transport failure can surface as *url.Error, and fmt.Errorf
		// would bake its raw URL into this message.
		return nil, scrubURLError(err, safe)
	}
	res.ETag = strings.Trim(resp.Header.Get("ETag"), `"`)
	return res, nil
}

// spoolDir reports the directory os.CreateTemp will use, for an error message
// that names a real path rather than an empty string.
func (d *Downloader) spoolDir() string {
	if d.tempDir != "" {
		return d.tempDir
	}
	return os.TempDir()
}

// writeSpool copies body into f, hashing in the same pass, and returns a Result
// holding f. On any error f is left untouched for the caller to discard — this
// function never closes or removes it, so ownership stays with exactly one
// place.
//
// The cap is enforced DURING the copy by reading one byte past it, which keeps
// the pre-SJ24K-53 detection semantics: an artifact exactly at the cap is valid
// and one byte over is deterministically rejected, rather than inferred from a
// truncated read. The overshoot is bounded at a single byte, so an over-cap
// artifact costs cap+1 bytes of transfer and disk rather than its full length —
// a 400 MB artifact under a 300 MB cap is abandoned at 300 MB, not downloaded.
func writeSpool(f *os.File, body io.Reader, maxBytes int64) (*Result, error) {
	sum := sha256.New()
	buf := make([]byte, copyBufferSize)
	n, err := io.CopyBuffer(io.MultiWriter(f, sum), io.LimitReader(body, maxBytes+1), buf)
	if err != nil {
		return nil, fmt.Errorf("spool body: %w", err)
	}
	if n > maxBytes {
		return nil, Permanent(fmt.Errorf("download exceeds the %d byte cap", maxBytes))
	}
	return &Result{
		Hash: hex.EncodeToString(sum.Sum(nil)),
		Size: n,
		file: f,
		path: f.Name(),
	}, nil
}

// discardSpool closes and deletes an abandoned spool file. Errors are ignored
// deliberately: this only ever runs on a path that is already returning a more
// informative error, and replacing that error with a cleanup failure would hide
// the cause.
func discardSpool(f *os.File) {
	_ = f.Close()
	_ = os.Remove(f.Name())
}
