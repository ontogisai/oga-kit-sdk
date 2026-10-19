package transfer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// fakeLoaderGateway records the loader calls it received on the gateway's
// loader endpoint (POST /loader/v1/{op}, the arguments as the body) so tests
// can assert on the request shape without standing up a real platform.
type fakeLoaderGateway struct {
	prepareCalls   int
	completeCalls  int
	toolCallHits   int // requests to the pre-v0.102 /mcp/tools/call shape
	lastPath       string
	lastTenantID   string
	lastKitID      string
	lastAuthHeader string
	lastBody       []byte
	lastUploadURL  string
	prepareToken   string
	uploadCalls    int
	uploadBody     []byte
}

func newFakeLoaderGateway(t *testing.T) (*fakeLoaderGateway, *httptest.Server, *httptest.Server) {
	t.Helper()
	g := &fakeLoaderGateway{
		prepareToken: "tok-fake-123",
	}

	// Storage server — the loader streams the artifact body here when
	// the writer chose presigned mode. We have it record the body for
	// later assertion.
	storageMux := http.NewServeMux()
	storageMux.HandleFunc("PUT /upload-target", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.uploadCalls++
		g.uploadBody = body
		w.WriteHeader(http.StatusOK)
	})
	storageSrv := httptest.NewServer(storageMux)
	t.Cleanup(storageSrv.Close)
	g.lastUploadURL = storageSrv.URL + "/upload-target"

	// Gateway server — the loader endpoint for prepare_upload + complete.
	record := func(r *http.Request) bool {
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			return false
		}
		g.lastPath = r.URL.Path
		g.lastTenantID = r.Header.Get("X-Tenant-ID")
		g.lastKitID = r.Header.Get("X-Kit-ID")
		g.lastAuthHeader = r.Header.Get("Authorization")
		g.lastBody = body
		return true
	}
	gatewayMux := http.NewServeMux()
	gatewayMux.HandleFunc("POST /loader/v1/prepare_upload", func(w http.ResponseWriter, r *http.Request) {
		if !record(r) {
			http.Error(w, "body is not JSON", http.StatusBadRequest)
			return
		}
		g.prepareCalls++
		_ = json.NewEncoder(w).Encode(transfer.PrepareUploadResponse{
			UploadURL:   g.lastUploadURL,
			UploadToken: g.prepareToken,
		})
	})
	gatewayMux.HandleFunc("POST /loader/v1/complete", func(w http.ResponseWriter, r *http.Request) {
		if !record(r) {
			http.Error(w, "body is not JSON", http.StatusBadRequest)
			return
		}
		g.completeCalls++
		_ = json.NewEncoder(w).Encode(transfer.CompleteResponse{
			JobID:  "job-platform-1",
			Status: "running",
		})
	})
	gatewayMux.HandleFunc("POST /mcp/tools/call", func(w http.ResponseWriter, _ *http.Request) {
		g.toolCallHits++
		http.Error(w, "the transfer client must not use the tool-call shape", http.StatusGone)
	})
	gatewaySrv := httptest.NewServer(gatewayMux)
	t.Cleanup(gatewaySrv.Close)

	return g, gatewaySrv, storageSrv
}

func TestHTTPCommitClient_PrepareUpload(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, err := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")
	if err != nil {
		t.Fatalf("NewHTTPCommitClient: %v", err)
	}

	resp, err := cc.PrepareUpload(context.Background())
	if err != nil {
		t.Fatalf("PrepareUpload: %v", err)
	}
	if resp.UploadToken != gate.prepareToken {
		t.Errorf("UploadToken = %q, want %q", resp.UploadToken, gate.prepareToken)
	}
	if gate.lastTenantID != "tenant-A" {
		t.Errorf("X-Tenant-ID header = %q, want tenant-A", gate.lastTenantID)
	}
	if gate.lastKitID != "test-kit" {
		t.Errorf("X-Kit-ID header = %q, want test-kit", gate.lastKitID)
	}
	if gate.lastPath != "/loader/v1/prepare_upload" {
		t.Errorf("path = %q, want /loader/v1/prepare_upload", gate.lastPath)
	}
	if string(gate.lastBody) != `{}` {
		t.Errorf("body = %s, want {} (prepare_upload takes no arguments)", gate.lastBody)
	}
	if gate.toolCallHits != 0 {
		t.Errorf("the client used the tool-call shape %d time(s)", gate.toolCallHits)
	}
}

func TestHTTPCommitClient_CompleteRejectsBothBodyForms(t *testing.T) {
	t.Parallel()
	_, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")

	cases := []struct {
		name string
		req  *transfer.CompleteRequest
	}{
		{"both", &transfer.CompleteRequest{
			UploadToken: "tok",
			InlineBody:  []byte("x"),
			Kind:        transfer.KindOntology,
			Format:      transfer.FormatNDJSON,
			ContentHash: "abc",
		}},
		{"neither", &transfer.CompleteRequest{
			Kind:        transfer.KindOntology,
			Format:      transfer.FormatNDJSON,
			ContentHash: "abc",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cc.Complete(context.Background(), tc.req)
			if err == nil {
				t.Errorf("Complete should reject %s body shape", tc.name)
			}
		})
	}
}

func TestHTTPCommitClient_PutBytes(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")

	prep, err := cc.PrepareUpload(context.Background())
	if err != nil {
		t.Fatalf("PrepareUpload: %v", err)
	}

	body := strings.NewReader(`{"format":"ndjson"}`)
	if err := cc.PutBytes(context.Background(), prep.UploadURL, body, int64(len(`{"format":"ndjson"}`))); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if gate.uploadCalls != 1 {
		t.Errorf("uploadCalls = %d, want 1", gate.uploadCalls)
	}
	if string(gate.uploadBody) != `{"format":"ndjson"}` {
		t.Errorf("uploaded body = %q, want %q", gate.uploadBody, `{"format":"ndjson"}`)
	}
}

// The writer uploads a spooled artifact as an io.SectionReader, which net/http
// cannot re-send on its own. A storage endpoint that answers 307 must still end
// up with every byte at the redirect target — the bytes.Reader the writer used
// before did, and the SectionReader did not until PutBytes set GetBody.
func TestHTTPCommitClient_PutBytesFollowsRedirectWithSectionReader(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("ndjson-line\n"), 100_000)
	var landed []byte
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /first", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, "/second", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("PUT /second", func(w http.ResponseWriter, r *http.Request) {
		landed, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cc, _ := transfer.NewHTTPCommitClient(srv.URL, "tenant-A", "test-kit")

	f, err := os.CreateTemp(t.TempDir(), "spool")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.Write(payload); err != nil {
		t.Fatal(err)
	}
	body := io.NewSectionReader(f, 0, int64(len(payload)))
	if err := cc.PutBytes(context.Background(), srv.URL+"/first", body, int64(len(payload))); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if !bytes.Equal(landed, payload) {
		t.Fatalf("redirect target received %d of %d bytes", len(landed), len(payload))
	}
}

// Only a 2xx is an upload. A 3xx the client did not follow means nothing landed,
// and must not be reported as success.
func TestHTTPCommitClient_PutBytesNon2xxIsFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusMultipleChoices, http.StatusNotModified, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(status)
		}))
		cc, _ := transfer.NewHTTPCommitClient(srv.URL, "tenant-A", "test-kit")
		err := cc.PutBytes(context.Background(), srv.URL+"/put", strings.NewReader("x"), 1)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Errorf("HTTP %d: err = %v, want a failure naming the status", status, err)
		}
	}
}

func TestHTTPCommitClient_GatewayErrorReturnsHTTPError(t *testing.T) {
	t.Parallel()
	gatewaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied"}`))
	}))
	t.Cleanup(gatewaySrv.Close)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")

	_, err := cc.PrepareUpload(context.Background())
	if err == nil {
		t.Fatal("expected error on 403")
	}
	var herr *transfer.HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if herr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", herr.StatusCode)
	}
}

func TestNewHTTPCommitClient_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		gatewayURL  string
		tenantID    string
		expectError bool
	}{
		{"missing gateway URL", "", "tenant", true},
		{"missing tenant ID", "http://gateway:8050", "", true},
		{"valid", "http://gateway:8050", "tenant", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := transfer.NewHTTPCommitClient(tc.gatewayURL, tc.tenantID, "kit")
			if (err != nil) != tc.expectError {
				t.Errorf("err = %v, expectError = %v", err, tc.expectError)
			}
		})
	}
}

func TestHTTPCommitClient_CompleteInline(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")

	resp, err := cc.Complete(context.Background(), &transfer.CompleteRequest{
		InlineBody:  []byte(`{"format":"ndjson"}`),
		Kind:        transfer.KindOntology,
		Format:      transfer.FormatNDJSON,
		ContentHash: "abc123",
		EntryCount:  1,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.JobID != "job-platform-1" {
		t.Errorf("JobID = %q, want job-platform-1", resp.JobID)
	}
	if gate.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1", gate.completeCalls)
	}
}

func TestHTTPCommitClient_CompletePresigned(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")

	resp, err := cc.Complete(context.Background(), &transfer.CompleteRequest{
		UploadToken: "tok-x",
		Kind:        transfer.KindData,
		Format:      transfer.FormatNDJSON,
		ContentHash: "def456",
		EntryCount:  100,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.JobID == "" {
		t.Error("JobID should not be empty")
	}
	if gate.lastPath != "/loader/v1/complete" {
		t.Errorf("path = %q, want /loader/v1/complete", gate.lastPath)
	}
	if gate.toolCallHits != 0 {
		t.Errorf("the client used the tool-call shape %d time(s)", gate.toolCallHits)
	}
}

// The body of loader.complete is the request itself, exactly as it marshals:
// no {"tool","params"} envelope. The platform reads it as the operation's
// arguments.
func TestHTTPCommitClient_CompleteBodyIsTheRequest(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")
	req := &transfer.CompleteRequest{
		InlineBody:  []byte(`{"format":"ndjson"}`),
		Kind:        transfer.KindData,
		Format:      transfer.FormatNDJSON,
		ContentHash: "abc123",
		EntryCount:  1,
		UpstreamRef: "run-7",
	}
	if _, err := cc.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gate.lastBody, want) {
		t.Errorf("body = %s\nwant %s", gate.lastBody, want)
	}
}

// An error answer from the loader endpoint is an HTTPError carrying the
// status, the endpoint's path and the platform's error payload as the body.
func TestHTTPCommitClient_EndpointErrorIsHTTPError(t *testing.T) {
	t.Parallel()
	const payload = `{"code":"OGA-CORE-DENY-1202","message":"denied","http_status":403}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loader/v1/complete" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	cc, _ := transfer.NewHTTPCommitClient(srv.URL, "tenant-A", "test-kit")
	_, err := cc.Complete(context.Background(), &transfer.CompleteRequest{InlineBody: json.RawMessage(`{"x":1}`)})
	var herr *transfer.HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T %v", err, err)
	}
	if herr.StatusCode != http.StatusForbidden || herr.Path != "/loader/v1/complete" || herr.Body != payload {
		t.Errorf("HTTPError = %+v", herr)
	}
}

func TestHTTPCommitClient_NilCompleteRequest(t *testing.T) {
	t.Parallel()
	cc, _ := transfer.NewHTTPCommitClient("http://x:8050", "t", "k")
	if _, err := cc.Complete(context.Background(), nil); err == nil {
		t.Error("Complete with nil request should fail")
	}
}

// TestHTTPCommitClient_WithTokenProvider verifies the OGA-404 workload-identity
// path: a token provider supplies the bearer the gateway sees on every call,
// without any token file.
func TestHTTPCommitClient_WithTokenProvider(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, err := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit",
		transfer.WithTokenProvider(func() string { return "minted-workload-jwt" }))
	if err != nil {
		t.Fatalf("NewHTTPCommitClient: %v", err)
	}

	if _, err := cc.Complete(context.Background(), &transfer.CompleteRequest{
		InlineBody: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gate.lastAuthHeader != "Bearer minted-workload-jwt" {
		t.Errorf("Authorization = %q, want %q", gate.lastAuthHeader, "Bearer minted-workload-jwt")
	}
}

// TestHTTPCommitClient_TokenProviderTakesPrecedence verifies the provider wins
// over a configured token path.
func TestHTTPCommitClient_TokenProviderTakesPrecedence(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, err := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit",
		transfer.WithTokenPath("/nonexistent/token/file"),
		transfer.WithTokenProvider(func() string { return "provider-wins" }))
	if err != nil {
		t.Fatalf("NewHTTPCommitClient: %v", err)
	}

	if _, err := cc.Complete(context.Background(), &transfer.CompleteRequest{
		InlineBody: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gate.lastAuthHeader != "Bearer provider-wins" {
		t.Errorf("Authorization = %q, want %q (provider must take precedence over token path)",
			gate.lastAuthHeader, "Bearer provider-wins")
	}
}

// TestHTTPCommitClient_NoTokenNoHeader verifies that with neither a provider nor
// a token path no Authorization header is sent (the gateway then answers 401).
func TestHTTPCommitClient_NoTokenNoHeader(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit")
	if _, err := cc.Complete(context.Background(), &transfer.CompleteRequest{
		InlineBody: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gate.lastAuthHeader != "" {
		t.Errorf("Authorization = %q, want empty (no token configured)", gate.lastAuthHeader)
	}
}

// TestHTTPCommitClient_EmptyProviderTokenNoHeader verifies a provider returning
// "" yields no Authorization header (rather than "Bearer ").
func TestHTTPCommitClient_EmptyProviderTokenNoHeader(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeLoaderGateway(t)
	cc, _ := transfer.NewHTTPCommitClient(gatewaySrv.URL, "tenant-A", "test-kit",
		transfer.WithTokenProvider(func() string { return "" }))
	if _, err := cc.Complete(context.Background(), &transfer.CompleteRequest{
		InlineBody: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gate.lastAuthHeader != "" {
		t.Errorf("Authorization = %q, want empty (provider returned empty token)", gate.lastAuthHeader)
	}
}

// An error answer is read up to 64 KiB, and the error text quotes at most
// 512 bytes of it: the platform's ceiling for a body quoted in an error
// (OGA-1036). A proxy in front of the gateway can answer with any size.
func TestHTTPCommitClient_ErrorBodyIsBounded(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 200<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(big))
	}))
	t.Cleanup(srv.Close)
	cc, _ := transfer.NewHTTPCommitClient(srv.URL, "tenant-A", "test-kit")
	_, err := cc.PrepareUpload(t.Context())
	herr, ok := errors.AsType[*transfer.HTTPError](err)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T %v", err, err)
	}
	if len(herr.Body) != 64<<10 {
		t.Errorf("Body holds %d bytes, want the 64 KiB read bound", len(herr.Body))
	}
	msg := herr.Error()
	if strings.Contains(msg, strings.Repeat("x", 513)) {
		t.Errorf("Error() quotes more than 512 bytes of the body (%d bytes)", len(msg))
	}
	if !strings.Contains(msg, "…(truncated)") {
		t.Errorf("Error() = %.80q…, want the truncation marker", msg)
	}
}
