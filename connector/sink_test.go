package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrSink_FailsLoudlyOnPoints(t *testing.T) {
	s := errSink{}
	if err := s.EmitPoints(context.Background(), nil); err != nil {
		t.Errorf("empty batch should be a no-op, got %v", err)
	}
	if err := s.EmitPoints(context.Background(), []Point{{Metric: "temp", Value: 1}}); err == nil {
		t.Error("emitting points with no sink configured must error, not silently drop")
	}
}

func TestHTTPTimeseriesSink_PostsBatchWithToken(t *testing.T) {
	var gotAuth, gotPath string
	var gotBatch timeseriesBatch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBatch)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sink := NewHTTPTimeseriesSink(srv.URL, func(context.Context) (string, error) { return "tok-123", nil })
	err := sink.EmitPoints(context.Background(), []Point{
		{SourceID: "sensor-1", Metric: "temperature", Unit: "C", Value: 22.5},
	})
	if err != nil {
		t.Fatalf("EmitPoints: %v", err)
	}
	if gotPath != IntakePath {
		t.Errorf("path = %q, want %q", gotPath, IntakePath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(gotBatch.Points) != 1 || gotBatch.Points[0].Metric != "temperature" {
		t.Errorf("batch = %+v", gotBatch)
	}
}

func TestHTTPTimeseriesSink_IntakeErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad mapping", http.StatusBadRequest)
	}))
	defer srv.Close()

	sink := NewHTTPTimeseriesSink(srv.URL, nil)
	err := sink.EmitPoints(context.Background(), []Point{{Metric: "temp", Value: 1}})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("expected intake error to surface, got %v", err)
	}
}
