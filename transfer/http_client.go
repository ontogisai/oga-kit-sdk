package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ontogisai/oga-kit-sdk/internal/redact"
)

// HTTPCommitClient is the production CommitClient. It calls the
// Platform Gateway's loader endpoint for prepare_upload + complete
// (POST {gateway}/loader/v1/{op}, the call's arguments as the body), and
// PUTs directly to MinIO/S3 for the presigned-upload step.
//
// Before v0.102.0-beta the client sent the operations as MCP tool calls
// ({"tool":"loader.complete","params":{...}} to /mcp/tools/call). The
// gateway still accepts that shape from older images for a transition
// period and logs each sidecar that uses it; a platform whose gateway
// predates the loader route answers this client 404.
//
// The client is goroutine-safe; share one instance across multiple
// writers.
type HTTPCommitClient struct {
	gatewayURL string
	tenantID   string
	kitID      string
	// tokenProvider, when set, is called on every request to obtain the
	// bearer token: the sidecar's workload token, from
	// auth.WorkloadTokenProviderFromEnv. When nil, no Authorization header is
	// sent and the gateway answers 401.
	tokenProvider func() string

	httpClient *http.Client
}

// HTTPCommitClientOption tunes an HTTPCommitClient.
type HTTPCommitClientOption func(*HTTPCommitClient)

// WithHTTPClient overrides the default *http.Client. Useful for
// plugging in custom transports (mTLS, retry middleware,
// instrumentation). The default has a 5-minute timeout per request.
func WithHTTPClient(hc *http.Client) HTTPCommitClientOption {
	return func(c *HTTPCommitClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout sets a per-request timeout on the default HTTP client.
// Ignored when [WithHTTPClient] supplies a custom client (set the
// timeout on your client instead).
func WithTimeout(d time.Duration) HTTPCommitClientOption {
	return func(c *HTTPCommitClient) {
		if c.httpClient != nil && d > 0 {
			c.httpClient.Timeout = d
		}
	}
}

// WithTokenProvider supplies a function called on every request to obtain the
// bearer token. Pass the provider from auth.WorkloadTokenProviderFromEnv,
// which mints the sidecar's workload token from its bootstrap identity and
// renews it in memory. Without a provider the client sends no Authorization
// header, and the Platform Gateway refuses the request with 401: it has no
// tokenless mode. A nil fn is ignored.
func WithTokenProvider(fn func() string) HTTPCommitClientOption {
	return func(c *HTTPCommitClient) {
		if fn != nil {
			c.tokenProvider = fn
		}
	}
}

// NewHTTPCommitClient constructs an HTTPCommitClient targeting the
// supplied gateway base URL. The tenantID is sent on every call as
// the X-Tenant-ID header. It is informational: the platform takes the
// tenant from the verified workload token and ignores the header.
//
// kitID is informational; the platform attributes loads to the
// installed kit by matching the tenant + auth context, not by the
// kit_id field.
func NewHTTPCommitClient(gatewayURL, tenantID, kitID string, opts ...HTTPCommitClientOption) (*HTTPCommitClient, error) {
	if gatewayURL == "" {
		return nil, errors.New("transfer.NewHTTPCommitClient: gatewayURL is required")
	}
	if tenantID == "" {
		return nil, errors.New("transfer.NewHTTPCommitClient: tenantID is required")
	}
	c := &HTTPCommitClient{
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		tenantID:   tenantID,
		kitID:      kitID,
		httpClient: &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Loader endpoint paths on the Platform Gateway. Each operation is a POST
// whose body is the operation's arguments; a 2xx body is the result. A
// status of 400 or more is returned as an [HTTPError] with the body as the
// platform answered: the loader endpoint's structured error payload (code,
// message, http_status, ...) when the operation ran, or the gateway's own
// refusal (unauthorized, tool_not_allowed, a rate limit, an open circuit)
// when it did not.
const (
	loaderPrepareUploadPath = "/loader/v1/prepare_upload"
	loaderCompletePath      = "/loader/v1/complete"
)

// maxErrorBodyRead is how much of an error answer is read. An error message
// quotes at most redact.MaxQuotedBody bytes of it (OGA-1036).
const maxErrorBodyRead = 64 << 10

// PrepareUpload calls loader.prepare_upload, which takes no arguments.
func (c *HTTPCommitClient) PrepareUpload(ctx context.Context) (*PrepareUploadResponse, error) {
	raw, err := c.postJSON(ctx, loaderPrepareUploadPath, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out PrepareUploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode loader.prepare_upload response: %w", err)
	}
	return &out, nil
}

// PutBytes streams the artifact body to the presigned PUT URL.
//
// The body must be re-sendable, because net/http re-sends it in two cases: to
// follow a 307/308 redirect, and to retry a request a dead pooled connection or a
// refused HTTP/2 stream never let it write. It does that through
// [http.Request.GetBody], which net/http fills in only for bytes and strings
// readers. The default writer hands over an [io.SectionReader] over its spool
// file, so GetBody is set here to re-read that section from its start. Without
// it, a redirected PUT came back as the bare 307 with nothing uploaded.
//
// Only a 2xx response is success. A 3xx that was not followed means the bytes
// did not land, and reporting it as success would let Close go on to commit an
// artifact that is not in storage.
func (c *HTTPCommitClient) PutBytes(ctx context.Context, uploadURL string, body io.Reader, size int64) error {
	if uploadURL == "" {
		return errors.New("transfer.PutBytes: uploadURL is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return fmt.Errorf("build presigned PUT request: %w", redact.ScrubURLError(err, uploadURL))
	}
	req.ContentLength = size
	if sr, ok := body.(*io.SectionReader); ok && req.GetBody == nil {
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(sr, 0, size)), nil
		}
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("presigned PUT: %w", redact.ScrubURLError(err, uploadURL))
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Summarized, never quoted: an S3 signature error echoes the canonical
		// request, security token included (OGA-1036).
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		return fmt.Errorf("presigned PUT failed: HTTP %d: %s",
			resp.StatusCode, redact.ErrorBodyMax(errBody, 256))
	}
	return nil
}

// Complete calls loader.complete with req as the body. The platform
// decides which dispatcher (ontology vs data) to route to based on
// req.Kind, accepts the artifact, and returns immediately with a
// platform-issued job_id.
func (c *HTTPCommitClient) Complete(ctx context.Context, req *CompleteRequest) (*CompleteResponse, error) {
	if req == nil {
		return nil, errors.New("transfer.Complete: nil request")
	}
	if (req.UploadToken == "") == (len(req.InlineBody) == 0) {
		return nil, errors.New("transfer.Complete: exactly one of upload_token or inline_body is required")
	}
	raw, err := c.postJSON(ctx, loaderCompletePath, req)
	if err != nil {
		return nil, err
	}
	var out CompleteResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode loader.complete response: %w", err)
	}
	if out.JobID == "" {
		return nil, errors.New("loader.complete: response missing job_id")
	}
	return &out, nil
}

// postJSON performs a POST against the gateway with the standard
// auth + tenant headers and returns the raw response body. A status of
// 400 or more is returned as an [HTTPError].
func (c *HTTPCommitClient) postJSON(ctx context.Context, path string, body any) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.gatewayURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-ID", c.tenantID)
	if c.kitID != "" {
		req.Header.Set("X-Kit-ID", c.kitID)
	}
	if token := c.loadToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", path, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode >= 400 {
		// Bounded: an error answer is not a result, and a proxy in front of
		// the gateway can answer with a body of any size.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(errBody)),
		}
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return respBody, nil
}

// loadToken returns the bearer token for a request, or "" (no Authorization
// header) when no token provider is configured.
func (c *HTTPCommitClient) loadToken() string {
	if c.tokenProvider == nil {
		return ""
	}
	return strings.TrimSpace(c.tokenProvider())
}

// HTTPError is the typed error returned for a status of 400 or more from
// the gateway.
type HTTPError struct {
	StatusCode int
	Path       string
	// Body is the response body as the platform answered, trimmed and read
	// up to 64 KiB, for a caller that parses the platform's structured
	// error. It is the upstream's text: log [HTTPError.Error], which quotes
	// a bounded summary of it, not Body.
	Body string
}

// Error quotes at most 512 bytes of the body (an S3-style XML error is
// reduced to its code), so the error is safe to log (OGA-1036).
func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("transfer %s: HTTP %d — %s", e.Path, e.StatusCode,
			redact.ErrorBody([]byte(e.Body)))
	}
	return fmt.Sprintf("transfer %s: HTTP %d", e.Path, e.StatusCode)
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
