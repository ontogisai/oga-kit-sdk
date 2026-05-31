package loader_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/loader"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// streamingStub implements StreamingLoaderHandler so we can verify
// that the SDK's HTTP server detects the streaming variant via type
// assertion and drives Plan / Pass instead of a single Load call.
type streamingStub struct {
	planFn func(ctx context.Context, lc *loader.LoadContext) (*loader.LoadPlan, error)
	passFn func(ctx context.Context, lc *loader.LoadContext, p *loader.PassSpec) error
	calls  []string
}

func (s *streamingStub) Load(_ context.Context, _ *loader.LoadContext) (*loader.LoadResponse, error) {
	s.calls = append(s.calls, "Load")
	return nil, errors.New("Load should not be called when StreamingLoaderHandler is implemented")
}

func (s *streamingStub) Plan(ctx context.Context, lc *loader.LoadContext) (*loader.LoadPlan, error) {
	s.calls = append(s.calls, "Plan")
	if s.planFn != nil {
		return s.planFn(ctx, lc)
	}
	return &loader.LoadPlan{
		Passes: []loader.PassSpec{
			{Name: "vertices", EntryKinds: []string{"vertex"}},
			{Name: "edges", EntryKinds: []string{"edge"}},
		},
	}, nil
}

func (s *streamingStub) Pass(ctx context.Context, lc *loader.LoadContext, p *loader.PassSpec) error {
	s.calls = append(s.calls, "Pass:"+p.Name)
	if s.passFn != nil {
		return s.passFn(ctx, lc, p)
	}
	return nil
}

func (s *streamingStub) Job(_ context.Context, jobID string) (*loader.LoadResponse, error) {
	return nil, &loader.ErrJobNotFound{JobID: jobID}
}
func (s *streamingStub) Formats(_ context.Context) ([]string, error) {
	return []string{"streaming-test"}, nil
}
func (s *streamingStub) Health(_ context.Context) (*loader.HealthResponse, error) {
	return &loader.HealthResponse{Status: "ok"}, nil
}

func TestServer_DetectsStreamingHandler(t *testing.T) {
	t.Parallel()
	stub := &streamingStub{}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter("nop-job"), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)
	c, err := loader.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := c.Load(context.Background(), &loader.LoadRequest{
		TenantID:  "t",
		SourceURI: "file:///x",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if resp.Status != loader.StatusRunning {
		t.Errorf("status = %q, want running (streaming handler returns running with platform job_id)", resp.Status)
	}

	wantCalls := []string{"Plan", "Pass:vertices", "Pass:edges"}
	if len(stub.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", stub.calls, wantCalls)
	}
	for i, want := range wantCalls {
		if stub.calls[i] != want {
			t.Errorf("call[%d] = %q, want %q", i, stub.calls[i], want)
		}
	}
}

func TestServer_StreamingPlanError(t *testing.T) {
	t.Parallel()
	stub := &streamingStub{
		planFn: func(_ context.Context, _ *loader.LoadContext) (*loader.LoadPlan, error) {
			return nil, errors.New("plan failed")
		},
	}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)
	c, _ := loader.NewClient(srv.URL)

	_, err := c.Load(context.Background(), &loader.LoadRequest{
		TenantID:  "t",
		SourceURI: "file:///x",
	})
	if err == nil {
		t.Fatal("expected error when Plan fails")
	}
	if !strings.Contains(err.Error(), "plan failed") {
		t.Errorf("error = %v, want contains 'plan failed'", err)
	}
}

func TestServer_StreamingPassError(t *testing.T) {
	t.Parallel()
	stub := &streamingStub{
		passFn: func(_ context.Context, _ *loader.LoadContext, p *loader.PassSpec) error {
			if p.Name == "edges" {
				return errors.New("ID resolution failed")
			}
			return nil
		},
	}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)
	c, _ := loader.NewClient(srv.URL)

	_, err := c.Load(context.Background(), &loader.LoadRequest{
		TenantID:  "t",
		SourceURI: "file:///x",
	})
	if err == nil {
		t.Fatal("expected error when Pass fails")
	}
	if !strings.Contains(err.Error(), "ID resolution failed") {
		t.Errorf("error = %v, want contains 'ID resolution failed'", err)
	}

	// Vertices pass should have completed before edges failed.
	if len(stub.calls) != 3 {
		t.Errorf("calls = %v, want [Plan Pass:vertices Pass:edges]", stub.calls)
	}
}

func TestServer_StreamingEmptyPlan(t *testing.T) {
	t.Parallel()
	stub := &streamingStub{
		planFn: func(_ context.Context, _ *loader.LoadContext) (*loader.LoadPlan, error) {
			return &loader.LoadPlan{}, nil
		},
	}
	factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
		return transfer.NewNopWriter(""), nil
	}
	srv := httptest.NewServer(loader.Handler(stub, loader.WithWriterFactory(factory)))
	t.Cleanup(srv.Close)
	c, _ := loader.NewClient(srv.URL)

	_, err := c.Load(context.Background(), &loader.LoadRequest{
		TenantID:  "t",
		SourceURI: "file:///x",
	})
	if err == nil {
		t.Fatal("expected error on empty plan")
	}
	if !strings.Contains(err.Error(), "empty_plan") {
		t.Errorf("error = %v, want contains 'empty_plan'", err)
	}
}
