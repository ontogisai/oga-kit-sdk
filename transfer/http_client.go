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
)

// HTTPCommitClient is the production CommitClient. It calls the
// platform's MCP gateway for prepare_upload + complete, and PUTs
// directly to MinIO/S3 for the presigned-upload step.
//
// The client is goroutine-safe; share one instance across multiple
// writers.
type HTTPCommitClient struct {
	gatewayURL string
	tenantID   string
	kitID      string
	tokenPath  string

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

// WithTokenPath points the client at a file the SDK reads on every
// request to obtain the bearer token. Leaving this empty puts the
// client in "no Authorization header" mode — fine for dev-mode
// gateways (OGA-228) but not for production deployments.
func WithTokenPath(path string) HTTPCommitClientOption {
	return func(c *HTTPCommitClient) {
		c.tokenPath = strings.TrimSpace(path)
	}
}

// NewHTTPCommitClient constructs an HTTPCommitClient targeting the
// supplied gateway base URL. The tenantID is sent on every call as
// the X-Tenant-ID header — the platform's gateway treats this header
// as authoritative; any tenant claim in a request body is stripped
// before dispatch.
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

// PrepareUpload calls the loader.prepare_upload MCP tool.
func (c *HTTPCommitClient) PrepareUpload(ctx context.Context) (*PrepareUploadResponse, error) {
	body := map[string]any{
		"tool":   "loader.prepare_upload",
		"params": map[string]any{},
	}
	raw, err := c.postJSON(ctx, "/mcp/tools/call", body)
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
func (c *HTTPCommitClient) PutBytes(ctx context.Context, uploadURL string, body io.Reader, size int64) error {
	if uploadURL == "" {
		return errors.New("transfer.PutBytes: uploadURL is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return fmt.Errorf("build presigned PUT request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("presigned PUT: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("presigned PUT failed: HTTP %d %s — %s",
			resp.StatusCode, resp.Status, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// Complete calls the loader.complete MCP tool. The platform decides
// which dispatcher (ontology vs data) to route to based on
// req.Kind, accepts the artifact, and returns immediately with a
// platform-issued job_id.
func (c *HTTPCommitClient) Complete(ctx context.Context, req *CompleteRequest) (*CompleteResponse, error) {
	if req == nil {
		return nil, errors.New("transfer.Complete: nil request")
	}
	if (req.UploadToken == "") == (len(req.InlineBody) == 0) {
		return nil, errors.New("transfer.Complete: exactly one of upload_token or inline_body is required")
	}
	body := map[string]any{
		"tool":   "loader.complete",
		"params": req,
	}
	raw, err := c.postJSON(ctx, "/mcp/tools/call", body)
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
// auth + tenant headers and returns the raw response body. Non-2xx
// responses are returned as a wrapped error containing the status
// code and body for kit-author debugging.
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(respBody)),
		}
	}
	return respBody, nil
}

// loadToken reads the bearer token from the configured file path on
// every request. Caching is intentional null for dev — production
// callers wire in a real token watcher via [WithHTTPClient].
func (c *HTTPCommitClient) loadToken() string {
	if c.tokenPath == "" {
		return ""
	}
	data, err := readFile(c.tokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// HTTPError is the typed error returned for non-2xx responses from
// the gateway.
type HTTPError struct {
	StatusCode int
	Path       string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("transfer %s: HTTP %d — %s", e.Path, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("transfer %s: HTTP %d", e.Path, e.StatusCode)
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
