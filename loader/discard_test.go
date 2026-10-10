package loader_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// recordingDiscarder wraps a NopWriter with transfer.Discarder, honouring the
// interface's contract (a no-op once closed), and records which ended it.
type recordingDiscarder struct {
	*transfer.NopWriter
	closed       bool
	discardCalls int
	released     bool // Discard ran before any Close
}

func (r *recordingDiscarder) Close(ctx context.Context) (*transfer.Receipt, error) {
	r.closed = true
	return r.NopWriter.Close(ctx)
}

func (r *recordingDiscarder) Discard() error {
	r.discardCalls++
	if !r.closed {
		r.released = true
	}
	return nil
}

// singlePassStub is a LoaderHandler whose Load result the test chooses.
type singlePassStub struct {
	resp *loader.LoadResponse
	err  error
}

func (s *singlePassStub) Load(context.Context, *loader.LoadContext) (*loader.LoadResponse, error) {
	return s.resp, s.err
}

func (s *singlePassStub) Job(_ context.Context, id string) (*loader.LoadResponse, error) {
	return nil, &loader.ErrJobNotFound{JobID: id}
}
func (s *singlePassStub) Formats(context.Context) ([]string, error) { return []string{"x"}, nil }
func (s *singlePassStub) Health(context.Context) (*loader.HealthResponse, error) {
	return &loader.HealthResponse{Status: "ok"}, nil
}

// Every way a load can end. A writer the server does not commit — a Load, Plan
// or Pass error, a nil response, an empty plan — must be discarded; a committed
// one must be closed and not discarded first.
func TestServer_DiscardsEveryUncommittedWriter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		impl        loader.LoaderHandler
		wantClosed  bool
		wantRelease bool
	}{
		{"load error", &singlePassStub{err: errors.New("bad source")}, false, true},
		{"nil response", &singlePassStub{}, false, true},
		{"load committed", &singlePassStub{resp: &loader.LoadResponse{Status: loader.StatusRunning}}, true, false},
		{"plan error", &streamingStub{planFn: func(context.Context, *loader.LoadContext) (*loader.LoadPlan, error) {
			return nil, errors.New("plan failed")
		}}, false, true},
		{"empty plan", &streamingStub{planFn: func(context.Context, *loader.LoadContext) (*loader.LoadPlan, error) {
			return &loader.LoadPlan{}, nil
		}}, false, true},
		{"pass error", &streamingStub{passFn: func(context.Context, *loader.LoadContext, *loader.PassSpec) error {
			return errors.New("pass failed")
		}}, false, true},
		{"streaming committed", &streamingStub{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &recordingDiscarder{NopWriter: transfer.NewNopWriter("")}
			factory := func(context.Context, transfer.LoadKind, *loader.LoadRequest) (transfer.Writer, error) {
				return w, nil
			}
			srv := httptest.NewServer(loader.Handler(tc.impl, loader.WithWriterFactory(factory)))
			t.Cleanup(srv.Close)
			c, _ := loader.NewClient(srv.URL)
			_, _ = c.Load(context.Background(), &loader.LoadRequest{TenantID: "t", SourceURI: "file:///x"})

			if w.closed != tc.wantClosed {
				t.Errorf("closed = %v, want %v", w.closed, tc.wantClosed)
			}
			if w.released != tc.wantRelease {
				t.Errorf("released by Discard = %v, want %v", w.released, tc.wantRelease)
			}
			if w.discardCalls != 1 {
				t.Errorf("Discard calls = %d, want exactly 1 (deferred on every exit)", w.discardCalls)
			}
		})
	}
}
