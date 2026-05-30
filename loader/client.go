package loader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP client for the loader contract. The platform's
// DataImportWorkflow uses it to drive loader sidecars; kit authors may use
// it from integration tests against their own sidecar.
//
// The client itself does no retrying — callers (e.g., Temporal activities)
// own the retry policy. Errors returned mirror the HTTP status: a non-2xx
// response yields an [HTTPError] with the decoded ErrorResponse body when
// the sidecar produced one.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// ClientOption tunes a Client at construction time.
type ClientOption func(*Client)

// WithHTTPClient overrides the default *http.Client. Useful for plugging in
// custom transports (mTLS, retry middleware, instrumentation).
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout sets a per-request timeout on the default HTTP client. When
// WithHTTPClient is also supplied, this option is ignored — set the timeout
// on your custom client instead.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if c.httpClient != nil && d > 0 {
			c.httpClient.Timeout = d
		}
	}
}

// NewClient constructs a Client targeting the given base URL (e.g.,
// "http://my-loader:8400"). The URL must include scheme and authority; path
// components are stripped.
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("loader.NewClient: baseURL is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse loader URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("loader URL must include scheme and host: %q", baseURL)
	}
	// Keep only scheme + host so trailing slashes / unintended paths in
	// callers don't break /load, /jobs/{id} etc.
	c := &Client{
		baseURL: u.Scheme + "://" + u.Host,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Load calls POST /load on the loader sidecar. Synchronous loaders return a
// terminal LoadResponse (Status=completed/failed); async loaders return a
// non-terminal response and the caller polls [Job].
func (c *Client) Load(ctx context.Context, req *LoadRequest) (*LoadResponse, error) {
	if req == nil {
		return nil, errors.New("loader.Client.Load: nil request")
	}
	if req.TenantID == "" {
		return nil, errors.New("loader.Client.Load: tenant_id is required")
	}
	if req.SourceURI == "" {
		return nil, errors.New("loader.Client.Load: source_uri is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal load request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/load", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build /load request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call /load: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, decodeHTTPError(resp, "/load")
	}

	var out LoadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /load response: %w", err)
	}
	return &out, nil
}

// Job calls GET /jobs/{id} on the loader sidecar. Returns [HTTPError] with
// StatusCode=404 when the job is unknown.
func (c *Client) Job(ctx context.Context, jobID string) (*LoadResponse, error) {
	if jobID == "" {
		return nil, errors.New("loader.Client.Job: jobID is required")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/jobs/"+url.PathEscape(jobID), nil)
	if err != nil {
		return nil, fmt.Errorf("build /jobs request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call /jobs/%s: %w", jobID, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, decodeHTTPError(resp, "/jobs/"+jobID)
	}

	var out LoadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /jobs response: %w", err)
	}
	return &out, nil
}

// Formats calls GET /formats on the loader sidecar.
func (c *Client) Formats(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/formats", nil)
	if err != nil {
		return nil, fmt.Errorf("build /formats request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call /formats: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, decodeHTTPError(resp, "/formats")
	}

	var out FormatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /formats response: %w", err)
	}
	return out.Formats, nil
}

// Health calls GET /healthz. Returns nil error and a HealthResponse with
// Status="ok" when ready. A non-OK response is returned as [HTTPError].
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/healthz", nil)
	if err != nil {
		return nil, fmt.Errorf("build /healthz request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call /healthz: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Try to decode the body as HealthResponse first (some loaders
		// return 503 with a body); fall back to HTTPError.
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			var hr HealthResponse
			if json.Unmarshal(body, &hr) == nil && hr.Status != "" {
				return &hr, &HTTPError{
					StatusCode: resp.StatusCode,
					Path:       "/healthz",
					Body:       &ErrorResponse{Message: hr.Message, Code: "unhealthy"},
				}
			}
		}
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Path:       "/healthz",
			Body:       decodeErrorBody(body),
		}
	}

	var out HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /healthz response: %w", err)
	}
	return &out, nil
}

// HTTPError is the typed error returned for non-2xx responses.
type HTTPError struct {
	// StatusCode is the HTTP status returned by the loader.
	StatusCode int

	// Path is the request path (for debugging).
	Path string

	// Body is the decoded ErrorResponse, when one was provided. May be
	// nil if the loader returned an empty or non-JSON body.
	Body *ErrorResponse
}

func (e *HTTPError) Error() string {
	if e.Body != nil && e.Body.Message != "" {
		if e.Body.Code != "" {
			return fmt.Sprintf("loader %s: %d %s (code=%s)",
				e.Path, e.StatusCode, e.Body.Message, e.Body.Code)
		}
		return fmt.Sprintf("loader %s: %d %s", e.Path, e.StatusCode, e.Body.Message)
	}
	return fmt.Sprintf("loader %s: HTTP %d", e.Path, e.StatusCode)
}

// IsNotFound reports whether err is an HTTPError with status 404.
func IsNotFound(err error) bool {
	var herr *HTTPError
	if errors.As(err, &herr) {
		return herr.StatusCode == http.StatusNotFound
	}
	return false
}

// --- helpers ---

func decodeHTTPError(resp *http.Response, path string) error {
	body, _ := io.ReadAll(resp.Body)
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Path:       path,
		Body:       decodeErrorBody(body),
	}
}

func decodeErrorBody(body []byte) *ErrorResponse {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var er ErrorResponse
	if err := json.Unmarshal(body, &er); err != nil {
		// Not JSON — surface the raw body as the message so callers
		// have something to log.
		return &ErrorResponse{Message: strings.TrimSpace(string(body))}
	}
	if er.Message == "" {
		return nil
	}
	return &er
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
