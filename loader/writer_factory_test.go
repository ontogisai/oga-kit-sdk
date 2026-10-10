package loader_test

// Coverage for the writer factory that SURVIVED the OGA-930 cut.
//
// snapshot_writer_test.go went with the reserved-config transport it tested, and
// nearly all of it was about an assertion that no longer exists. What remains is the
// part that never had anything to do with snapshots — a kit not hand-writing a
// factory closure — so it is tested here on its own terms.
//
// Driven through the REAL chassis rather than by calling a factory directly: the
// factory is installed on an unexported config, so HTTP is both the only way in and
// the path a kit actually uses. It also makes the 400-vs-500 mapping assertable end
// to end rather than by inspecting an error value.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// wfHandler emits one vertex and one edge — the minimum that forces the writer to
// serialize its header, which is written lazily on the first record.
type wfHandler struct{}

func (wfHandler) Load(ctx context.Context, lc *loader.LoadContext) (*loader.LoadResponse, error) {
	if err := lc.Transfer.WriteVertex(ctx, transfer.Vertex{ID: "src-01", EntityType: "Equipment"}); err != nil {
		return nil, err
	}
	if err := lc.Transfer.WriteEdge(ctx, transfer.Edge{
		RelationshipType: "feeds", SourceID: "src-01", TargetID: "src-02",
	}); err != nil {
		return nil, err
	}
	return &loader.LoadResponse{Status: "completed"}, nil
}
func (wfHandler) Job(context.Context, string) (*loader.LoadResponse, error) { return nil, nil }
func (wfHandler) Formats(context.Context) ([]string, error)                 { return []string{"ndjson"}, nil }
func (wfHandler) Health(context.Context) (*loader.HealthResponse, error) {
	return &loader.HealthResponse{Status: "ok"}, nil
}

// wfRunLoad drives a real POST /load and returns the response plus the artifact the
// commit client received.
func wfRunLoad(t *testing.T, cfgMap map[string]any, opts ...loader.HandlerOption) (*http.Response, string) {
	t.Helper()
	srv := httptest.NewServer(loader.Handler(wfHandler{}, opts...))
	t.Cleanup(srv.Close)

	body, err := json.Marshal(map[string]any{
		"tenant_id":  "tnt1",
		"source_uri": "file:///tmp/x.json",
		"config":     cfgMap,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/load", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tnt1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /load: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, string(raw)
}

// wfHeaderLine returns the artifact's raw header line, for assertions that must see
// the BYTES rather than a decoded struct.
func wfHeaderLine(t *testing.T, fc *transfer.FakeCommitClient) []byte {
	t.Helper()
	line, _, found := bytes.Cut(fc.LastBody(), []byte("\n"))
	if !found {
		t.Fatalf("artifact has no header line; body=%q", fc.LastBody())
	}
	return line
}

// WithCommitClient produces a working writer and stamps the kit id — its whole job
// now that the assertion is gone.
func TestWithCommitClient_ProducesAWorkingWriter(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	resp, body := wfRunLoad(t, nil,
		loader.WithCommitClient(fc, "example-kit"),
		loader.WithLoaderKind(transfer.KindData),
	)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 200/202; body=%s", resp.StatusCode, body)
	}
	var h transfer.Header
	if err := json.Unmarshal(wfHeaderLine(t, fc), &h); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if h.KitID != "example-kit" {
		t.Errorf("KitID = %q, want example-kit", h.KitID)
	}
	// The kind comes from WithLoaderKind, not the request — the reason the closure
	// this replaces could discard the request entirely.
	if h.Kind != transfer.KindData {
		t.Errorf("Kind = %q, want %q", h.Kind, transfer.KindData)
	}
	if h.FormatVersion != transfer.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", h.FormatVersion, transfer.FormatVersion)
	}
}

// The artifact header carries NO edge-completeness assertion, and there is no longer
// any way for a kit to put one there.
//
// This is the observable half of the OGA-930 cut: the platform resolves the assertion
// from its own state keyed on the submitter's verified identity, so a field on the
// artifact would be a second — unauthenticated — source of the same claim.
//
// Asserted on the RAW header bytes. transfer.Header no longer HAS the field, so
// decoding into it could not reveal a stray one; only the bytes can.
func TestWithCommitClient_HeaderCarriesNoAssertion(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	// The formerly-reserved keys are now ordinary kit config and must be inert —
	// neither honored nor rejected.
	resp, body := wfRunLoad(t, map[string]any{
		"oga.full_snapshot":       true,
		"oga.governed_predicates": []any{"feeds"},
	},
		loader.WithCommitClient(fc, "example-kit"),
		loader.WithLoaderKind(transfer.KindData),
	)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a formerly-reserved config key must not fail the load: %d %s", resp.StatusCode, body)
	}
	if line := wfHeaderLine(t, fc); bytes.Contains(line, []byte("edge_completeness")) {
		t.Fatalf("the header still carries an assertion: %s", line)
	}
}

// A nil commit client fails at the FACTORY, as a 500, and never reaches the writer.
//
// Two things matter here and both are easy to lose. It must not reach
// transfer.NewWriter, where a nil client survives construction and panics at Close —
// after the kit has already done the entire load. And it is a DEPLOYMENT fault, so it
// must surface as 500 (retried, escalated) and not 400, which would tell an operator
// to fix a request that was fine.
func TestWithCommitClient_NilClientIsAServerFault(t *testing.T) {
	t.Parallel()
	resp, body := wfRunLoad(t, nil,
		loader.WithCommitClient(nil, "example-kit"),
		loader.WithLoaderKind(transfer.KindData),
	)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a deployment fault, not a bad request); body=%s",
			resp.StatusCode, body)
	}
	if !strings.Contains(body, "commit client") {
		t.Errorf("the error should name what is missing, got: %s", body)
	}
	// Not the 400 lane.
	if strings.Contains(body, "invalid_load_config") {
		t.Error("a nil client must not be reported as an invalid request")
	}
}

// ErrInvalidLoadConfig from a kit's own factory still maps to 400.
//
// This is the reason the sentinel outlived the snapshot-config parsing that used to
// be its only producer: the 400-vs-500 split is part of the kit-facing contract, and
// a custom factory rejecting a kit-defined config value is the remaining legitimate
// producer. Without a test, nothing would notice the branch going dead.
func TestErrInvalidLoadConfig_FromACustomFactoryIsABadRequest(t *testing.T) {
	t.Parallel()
	resp, body := wfRunLoad(t, nil,
		loader.WithWriterFactory(func(context.Context, transfer.LoadKind, *loader.LoadRequest) (transfer.Writer, error) {
			return nil, fmt.Errorf("%w: batch_size must be positive", loader.ErrInvalidLoadConfig)
		}),
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invalid_load_config") {
		t.Errorf("response should carry the invalid_load_config code, got: %s", body)
	}
	if !strings.Contains(body, "batch_size") {
		t.Errorf("the kit's own reason should reach the caller, got: %s", body)
	}
}

// A plain factory error stays 500, so the 400 above is a real discrimination rather
// than every factory failure being reported as a bad request.
func TestWriterFactoryError_WithoutTheSentinelIsAServerFault(t *testing.T) {
	t.Parallel()
	resp, body := wfRunLoad(t, nil,
		loader.WithWriterFactory(func(context.Context, transfer.LoadKind, *loader.LoadRequest) (transfer.Writer, error) {
			return nil, errors.New("gateway unreachable")
		}),
	)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, body)
	}
}

// Last-one-wins: a custom factory after WithCommitClient replaces it outright.
func TestWithWriterFactory_OverridesWithCommitClient(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	resp, body := wfRunLoad(t, nil,
		loader.WithCommitClient(fc, "example-kit"),
		loader.WithWriterFactory(func(context.Context, transfer.LoadKind, *loader.LoadRequest) (transfer.Writer, error) {
			return nil, errors.New("custom factory ran")
		}),
	)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "custom factory ran") {
		t.Fatalf("the later factory must win; got %d %s", resp.StatusCode, body)
	}
	if fc.CompleteCalls() != 0 {
		t.Error("the replaced commit client must not have been used")
	}
}
