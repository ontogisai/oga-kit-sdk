package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// fakeMCPGateway records the MCP tool calls it received so tests can
// assert on the request shape without standing up a real platform.
type fakeMCPGateway struct {
	prepareCalls   int
	completeCalls  int
	lastTool       string
	lastTenantID   string
	lastKitID      string
	lastAuthHeader string
	lastBody       []byte
	lastUploadURL  string
	prepareToken   string
	uploadCalls    int
	uploadBody     []byte
}

func newFakeMCPGateway(t *testing.T) (*fakeMCPGateway, *httptest.Server, *httptest.Server) {
	t.Helper()
	g := &fakeMCPGateway{
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

	// Gateway server — handles loader.prepare_upload + loader.complete.
	gatewayMux := http.NewServeMux()
	gatewayMux.HandleFunc("POST /mcp/tools/call", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tool   string          `json:"tool"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.lastTool = body.Tool
		g.lastTenantID = r.Header.Get("X-Tenant-ID")
		g.lastKitID = r.Header.Get("X-Kit-ID")
		g.lastAuthHeader = r.Header.Get("Authorization")
		g.lastBody = body.Params

		switch body.Tool {
		case "loader.prepare_upload":
			g.prepareCalls++
			resp := transfer.PrepareUploadResponse{
				UploadURL:   g.lastUploadURL,
				UploadToken: g.prepareToken,
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "loader.complete":
			g.completeCalls++
			resp := transfer.CompleteResponse{
				JobID:  "job-platform-1",
				Status: "running",
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "unknown tool", http.StatusNotFound)
		}
	})
	gatewaySrv := httptest.NewServer(gatewayMux)
	t.Cleanup(gatewaySrv.Close)

	return g, gatewaySrv, storageSrv
}

func TestHTTPCommitClient_PrepareUpload(t *testing.T) {
	t.Parallel()
	gate, gatewaySrv, _ := newFakeMCPGateway(t)
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
	if gate.lastTool != "loader.prepare_upload" {
		t.Errorf("tool = %q, want loader.prepare_upload", gate.lastTool)
	}
}

func TestHTTPCommitClient_CompleteRejectsBothBodyForms(t *testing.T) {
	t.Parallel()
	_, gatewaySrv, _ := newFakeMCPGateway(t)
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
	gate, gatewaySrv, _ := newFakeMCPGateway(t)
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
	gate, gatewaySrv, _ := newFakeMCPGateway(t)
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
	gate, gatewaySrv, _ := newFakeMCPGateway(t)
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
	if gate.lastTool != "loader.complete" {
		t.Errorf("lastTool = %q, want loader.complete", gate.lastTool)
	}
}

func TestHTTPCommitClient_NilCompleteRequest(t *testing.T) {
	t.Parallel()
	cc, _ := transfer.NewHTTPCommitClient("http://x:8050", "t", "k")
	if _, err := cc.Complete(context.Background(), nil); err == nil {
		t.Error("Complete with nil request should fail")
	}
}
