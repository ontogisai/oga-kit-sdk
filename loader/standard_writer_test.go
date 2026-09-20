package loader_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// --- helpers ---------------------------------------------------------------

// writingHandler emits one vertex and one edge, which is the minimum that forces
// the writer to serialize its header (written lazily on the first record).
type writingHandler struct{}

func (writingHandler) Load(ctx context.Context, lc *loader.LoadContext) (*loader.LoadResponse, error) {
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

func (writingHandler) Job(context.Context, string) (*loader.LoadResponse, error) { return nil, nil }
func (writingHandler) Formats(context.Context) ([]string, error)                 { return []string{"ndjson"}, nil }

func (writingHandler) Health(context.Context) (*loader.HealthResponse, error) {
	return &loader.HealthResponse{Status: "ok"}, nil
}

// artifactHeader pulls the first NDJSON line off a captured artifact body.
func artifactHeader(t *testing.T, body []byte) transfer.Header {
	t.Helper()
	line, _, found := bytes.Cut(body, []byte("\n"))
	if !found {
		t.Fatalf("artifact has no header line; body=%q", body)
	}
	var h transfer.Header
	if err := json.Unmarshal(line, &h); err != nil {
		t.Fatalf("decode header %q: %v", line, err)
	}
	return h
}

// runLoad drives a real /load request through the real chassis and returns the
// response plus the artifact the commit client received.
func runLoad(t *testing.T, cfgMap map[string]any, opts ...loader.HandlerOption) (*http.Response, *transfer.FakeCommitClient) {
	t.Helper()
	fc := &transfer.FakeCommitClient{}
	all := append([]loader.HandlerOption{
		loader.WithCommitClient(fc, "example-kit",
			loader.WithGovernedPredicates("feeds", "hasPoint")),
	}, opts...)
	srv := httptest.NewServer(loader.Handler(writingHandler{}, all...))
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
	return resp, fc
}

// --- the byte-identity guarantee -----------------------------------------

// A request carrying no reserved keys must produce an artifact byte-identical to
// one from the hand-written factory this replaces. Asserted on BYTES, not on the
// decoded header: a header field that serializes as an empty object rather than
// being omitted would satisfy a field-by-field comparison while changing the
// content hash the platform records.
func TestStandardWriterFactory_NoAssertionIsByteIdentical(t *testing.T) {
	t.Parallel()

	write := func(w transfer.Writer) {
		ctx := context.Background()
		if err := w.WriteVertex(ctx, transfer.Vertex{ID: "src-01", EntityType: "Equipment"}); err != nil {
			t.Fatalf("WriteVertex: %v", err)
		}
		if _, err := w.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// The legacy shape: exactly the closure every kit hand-wrote.
	legacyFC := &transfer.FakeCommitClient{}
	legacy := func(_ context.Context, kind transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewWriter(legacyFC, kind, "example-kit"), nil
	}
	lw, err := legacy(context.Background(), transfer.KindData, &loader.LoadRequest{})
	if err != nil {
		t.Fatalf("legacy factory: %v", err)
	}
	write(lw)

	// The standard factory, with a declared vocabulary present but no operator
	// assertion — the declaration alone must change nothing on the wire.
	stdFC := &transfer.FakeCommitClient{}
	std := loader.NewStandardWriterFactory(stdFC, "example-kit",
		loader.WithGovernedPredicates("feeds", "hasPoint"))
	sw, err := std(context.Background(), transfer.KindData, &loader.LoadRequest{
		Config: map[string]any{"batch_size": 100},
	})
	if err != nil {
		t.Fatalf("standard factory: %v", err)
	}
	write(sw)

	if !bytes.Equal(legacyFC.LastBody(), stdFC.LastBody()) {
		t.Fatalf("artifact bytes differ.\nlegacy:   %q\nstandard: %q",
			legacyFC.LastBody(), stdFC.LastBody())
	}
	if legacyFC.LastBodyHash() != stdFC.LastBodyHash() {
		t.Errorf("content hash differs: legacy=%s standard=%s",
			legacyFC.LastBodyHash(), stdFC.LastBodyHash())
	}
}

// --- factory behavior ----------------------------------------------------

func TestStandardWriterFactory_NilRequestIsTolerated(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	f := loader.NewStandardWriterFactory(fc, "example-kit")
	if _, err := f(context.Background(), transfer.KindData, nil); err != nil {
		t.Fatalf("nil request should not error: %v", err)
	}
}

// A nil commit client is a DEPLOYMENT fault, not a bad request: it must not be
// reported as 400, and it must not reach transfer.NewWriter, where it would
// survive construction and panic at Close after the kit had done the whole load.
func TestStandardWriterFactory_NilCommitClientIsNotAClientError(t *testing.T) {
	t.Parallel()
	f := loader.NewStandardWriterFactory(nil, "example-kit")
	w, err := f(context.Background(), transfer.KindData, &loader.LoadRequest{})
	if err == nil {
		t.Fatal("expected an error for a nil commit client")
	}
	if w != nil {
		t.Error("no writer should be returned")
	}
	if isInvalidLoadConfig(err) {
		t.Error("a nil commit client must NOT be classified as an invalid request (it would answer 400)")
	}
}

// The factory is called once per /load and appends to its option slice. Sharing
// the backing array would leak one request's assertion into the next — a request
// that asserted nothing would inherit the previous request's governed predicates
// and start closing edges.
func TestStandardWriterFactory_AssertionDoesNotLeakBetweenRequests(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	f := loader.NewStandardWriterFactory(fc, "example-kit",
		loader.WithGovernedPredicates("feeds"))
	ctx := context.Background()

	// Request 1 asserts.
	w1, err := f(ctx, transfer.KindData, &loader.LoadRequest{
		Config: map[string]any{loader.ConfigKeyFullSnapshot: true},
	})
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if err := w1.WriteVertex(ctx, transfer.Vertex{ID: "a", EntityType: "T"}); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := w1.Close(ctx); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if h := artifactHeader(t, fc.LastBody()); !h.EdgeCompleteness.Asserted() {
		t.Fatal("request 1 should have asserted")
	}

	// Request 2 does not.
	w2, err := f(ctx, transfer.KindData, &loader.LoadRequest{})
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if err := w2.WriteVertex(ctx, transfer.Vertex{ID: "b", EntityType: "T"}); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if _, err := w2.Close(ctx); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if h := artifactHeader(t, fc.LastBody()); h.EdgeCompleteness != nil {
		t.Fatalf("request 2 inherited an assertion: %+v", h.EdgeCompleteness)
	}
}

func TestStandardWriterFactory_PassesThroughWriterOptions(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	// A nil WriterOption is ignored by NewWriter; passing one proves the
	// pass-through slice is actually threaded rather than dropped.
	f := loader.NewStandardWriterFactory(fc, "example-kit",
		loader.WithWriterOptions(nil))
	if _, err := f(context.Background(), transfer.KindData, &loader.LoadRequest{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- end to end through the real chassis ---------------------------------

// The operator's flag has to reach the artifact header through the real HTTP
// handler, not a hand-built LoadRequest: the config map travels JSON-decoded, so
// the predicate list arrives as []any and the flag as a bool.
func TestLoad_OperatorAssertionReachesTheHeader(t *testing.T) {
	t.Parallel()
	resp, fc := runLoad(t, map[string]any{
		loader.ConfigKeyFullSnapshot: true,
	}, loader.WithLoaderKind(transfer.KindData))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 200/202", resp.StatusCode)
	}
	h := artifactHeader(t, fc.LastBody())
	if !h.EdgeCompleteness.Asserted() {
		t.Fatalf("header carries no assertion: %+v", h.EdgeCompleteness)
	}
	if got := h.EdgeCompleteness.GovernedPredicates; len(got) != 2 || got[0] != "feeds" || got[1] != "hasPoint" {
		t.Errorf("governed predicates = %v, want [feeds hasPoint]", got)
	}
}

func TestLoad_OperatorOverrideReachesTheHeader(t *testing.T) {
	t.Parallel()
	_, fc := runLoad(t, map[string]any{
		loader.ConfigKeyFullSnapshot: true,
		// Arrives as []any after JSON decode, with whitespace and a duplicate to
		// prove normalization runs on the wire shape.
		loader.ConfigKeyGovernedPredicates: []any{" hasPoint ", "hasPoint"},
	}, loader.WithLoaderKind(transfer.KindData))

	h := artifactHeader(t, fc.LastBody())
	if got := h.EdgeCompleteness.GovernedPredicates; len(got) != 1 || got[0] != "hasPoint" {
		t.Fatalf("governed predicates = %v, want [hasPoint]", got)
	}
}

func TestLoad_NoAssertionLeavesHeaderClean(t *testing.T) {
	t.Parallel()
	_, fc := runLoad(t, map[string]any{"batch_size": 10}, loader.WithLoaderKind(transfer.KindData))
	if h := artifactHeader(t, fc.LastBody()); h.EdgeCompleteness != nil {
		t.Fatalf("header carries an assertion nobody made: %+v", h.EdgeCompleteness)
	}
}

// A malformed reserved key is a CLIENT error. Before this it was a 500, which
// reads as a platform fault and gets triaged as one while the remedy is to fix
// the request.
func TestLoad_MalformedConfigAnswers400AndWritesNothing(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"quoted true": {
			loader.ConfigKeyFullSnapshot: "true",
		},
		"predicates without the assertion": {
			loader.ConfigKeyGovernedPredicates: []any{"feeds"},
		},
		"assertion false with predicates": {
			loader.ConfigKeyFullSnapshot:       false,
			loader.ConfigKeyGovernedPredicates: []any{"feeds"},
		},
		"non-string predicate": {
			loader.ConfigKeyFullSnapshot:       true,
			loader.ConfigKeyGovernedPredicates: []any{"feeds", 7},
		},
		"predicates not a list": {
			loader.ConfigKeyFullSnapshot:       true,
			loader.ConfigKeyGovernedPredicates: "feeds",
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resp, fc := runLoad(t, cfg, loader.WithLoaderKind(transfer.KindData))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var er loader.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if er.Code != "invalid_load_config" {
				t.Errorf("code = %q, want invalid_load_config", er.Code)
			}
			// Nothing may be committed: a refusal has to leave the platform with
			// no artifact at all, not a partial one.
			if fc.CompleteCalls() != 0 {
				t.Errorf("committed %d artifact(s) on a refused request", fc.CompleteCalls())
			}
		})
	}
}

// The declaration is absent, so the assertion cannot be honored. Refuse, naming
// both the kit-side fix and the per-run workaround.
func TestLoad_AssertionWithoutAnyVocabularyAnswers400(t *testing.T) {
	t.Parallel()
	fc := &transfer.FakeCommitClient{}
	srv := httptest.NewServer(loader.Handler(writingHandler{},
		// No WithGovernedPredicates.
		loader.WithCommitClient(fc, "example-kit"),
		loader.WithLoaderKind(transfer.KindData),
	))
	t.Cleanup(srv.Close)

	body := `{"tenant_id":"tnt1","source_uri":"file:///tmp/x.json","config":{"oga.full_snapshot":true}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/load", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Tenant-ID", "tnt1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /load: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var er loader.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(er.Message, "WithGovernedPredicates") {
		t.Errorf("message should name the missing declaration, got %q", er.Message)
	}
}

// An ontology loader cannot carry an assertion. Refused at the factory, so no
// artifact is started.
func TestLoad_AssertionOnOntologyLoaderAnswers400(t *testing.T) {
	t.Parallel()
	resp, fc := runLoad(t, map[string]any{
		loader.ConfigKeyFullSnapshot: true,
	}, loader.WithLoaderKind(transfer.KindOntology))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fc.CompleteCalls() != 0 {
		t.Errorf("committed %d artifact(s) for a refused ontology assertion", fc.CompleteCalls())
	}
}

// A loader with no writer configured at all still fails as a server fault, not a
// client one — the pre-existing behavior must not regress into a 400.
func TestLoad_MissingWriterFactoryStillAnswers500(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(loader.Handler(writingHandler{}))
	t.Cleanup(srv.Close)

	body := `{"tenant_id":"tnt1","source_uri":"file:///tmp/x.json"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/load", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Tenant-ID", "tnt1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /load: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// isInvalidLoadConfig mirrors what the chassis checks, without re-exporting
// errors.Is at every call site.
func isInvalidLoadConfig(err error) bool {
	return err != nil && strings.Contains(err.Error(), loader.ErrInvalidLoadConfig.Error())
}
