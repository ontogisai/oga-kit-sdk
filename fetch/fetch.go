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
//   - a size cap, so a hostile or mis-configured source cannot exhaust memory
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
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultMaxBytes caps a downloaded artifact at 64 MiB — generous for a
// published catalogue or export, bounded against a hostile source, and matching
// the platform intake's own body cap so a fetch that succeeds here is not
// rejected downstream for size.
const DefaultMaxBytes int64 = 64 << 20

// DefaultTimeout bounds a single attempt end-to-end.
const DefaultTimeout = 2 * time.Minute

// Result carries the fetched bytes plus the source's entity tag, which a poll
// loop can use for cheap change detection before paying for a full parse.
type Result struct {
	Body []byte
	ETag string
}

// Downloader fetches artifacts under the guards described on the package.
// Construct with New; the zero value is not usable.
type Downloader struct {
	http          *http.Client
	maxBytes      int64
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
// 2 minute per-attempt timeout, bounded retry, no allowlist, no credential.
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

// Get fetches rawURL under the configured guards.
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
		r, ferr := d.attempt(ctx, u.String(), safe)
		if ferr != nil {
			return ferr
		}
		res = r
		return nil
	})
	if err != nil {
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

	// Read one byte past the cap so an over-limit body is detected
	// deterministically rather than inferred from a truncated read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, d.maxBytes+1))
	if err != nil {
		// Scrubbed before the wrap, for the reason given on scrubURLError: a
		// mid-body transport failure can surface as *url.Error, and fmt.Errorf
		// would bake its raw URL into this message.
		return nil, fmt.Errorf("read body: %w", scrubURLError(err, safe))
	}
	if int64(len(body)) > d.maxBytes {
		return nil, Permanent(fmt.Errorf("download exceeds the %d byte cap", d.maxBytes))
	}
	return &Result{Body: body, ETag: strings.Trim(resp.Header.Get("ETag"), `"`)}, nil
}
