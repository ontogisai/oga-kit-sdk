package loader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/kitlog"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Property 10 (loader seam): a nil ServerConfig.Logger defaults to the
// identity-seeded kitlog.Default(); an explicit Logger is preserved unchanged.
func TestServerConfig_Defaults_LoggerSeam(t *testing.T) {
	c := &ServerConfig{}
	c.defaults()
	if c.Logger != kitlog.Default() {
		t.Fatalf("nil Logger should default to kitlog.Default(), got %p", c.Logger)
	}

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	c2 := &ServerConfig{Logger: custom}
	c2.defaults()
	if c2.Logger != custom {
		t.Fatalf("explicit Logger must be preserved unchanged")
	}
}

// failingDiscarder is a NopWriter whose Discard fails, so the handler has a
// warning to log.
type failingDiscarder struct{ *transfer.NopWriter }

func (failingDiscarder) Discard() error { return errors.New("spool release failed") }

type failingLoad struct{}

func (failingLoad) Load(context.Context, *LoadContext) (*LoadResponse, error) {
	return nil, errors.New("bad source")
}
func (failingLoad) Job(_ context.Context, id string) (*LoadResponse, error) {
	return nil, &ErrJobNotFound{JobID: id}
}
func (failingLoad) Formats(context.Context) ([]string, error) { return nil, nil }
func (failingLoad) Health(context.Context) (*HealthResponse, error) {
	return &HealthResponse{Status: "ok"}, nil
}

// The handler's own warnings — here, a writer it could not release — go to the
// logger ListenAndServe hands it from ServerConfig.Logger, not to the process
// default, so a kit's configured logger sees everything the server logs.
func TestHandler_WarnsThroughTheConfiguredLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	factory := func(context.Context, transfer.LoadKind, *LoadRequest) (transfer.Writer, error) {
		return failingDiscarder{transfer.NewNopWriter("")}, nil
	}
	srv := httptest.NewServer(Handler(failingLoad{}, withLogger(logger), WithWriterFactory(factory)))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/load", strings.NewReader(`{"source_uri":"file:///x"}`))
	req.Header.Set("X-Tenant-ID", "t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(buf.String(), "releasing an uncommitted writer failed") {
		t.Fatalf("configured logger did not receive the release warning; got %q", buf.String())
	}
}
