package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInvokeAgentStream_ParsesSSE verifies InvokeAgentStream parses SSE framing
// (event:/data:/blank-line) and yields each event's data JSON, skipping the
// [DONE] sentinel, comments, and event: lines (OGA-419 G3).
func TestInvokeAgentStream_ParsesSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("expected Accept: text/event-stream, got %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// A comment, two framed events (with event: lines), then the sentinel.
		_, _ = fmt.Fprint(w, ": connected\n\n")
		_, _ = fmt.Fprint(w, "event: task/reasoning\ndata: {\"type\":\"task/reasoning\",\"payload\":{\"text\":\"thinking\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: task/artifact\ndata: {\"type\":\"task/artifact\",\"payload\":{\"parts\":[{\"text\":\"answer\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := NewPlatformGatewayClient(srv.URL, "", "tenant-1")
	ch, err := c.InvokeAgentStream(context.Background(), "knowledge-agent", map[string]any{"role": "user"})
	if err != nil {
		t.Fatalf("InvokeAgentStream: %v", err)
	}

	var types []string
	for raw := range ch {
		var evt struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(*raw, &evt); err != nil {
			t.Fatalf("yielded non-JSON data: %q (%v)", string(*raw), err)
		}
		types = append(types, evt.Type)
	}

	// Exactly the two real events; [DONE], the comment, and event: lines are skipped.
	if len(types) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(types), types)
	}
	if types[0] != "task/reasoning" || types[1] != "task/artifact" {
		t.Errorf("unexpected event types: %v", types)
	}
}

// TestInvokeAgentStream_MultiLineData verifies multiple data: lines for one
// event are joined with newlines before decoding.
func TestInvokeAgentStream_MultiLineData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// JSON split across two data: lines (valid per SSE — joined with \n).
		_, _ = fmt.Fprint(w, "data: {\"type\":\"task/artifact\",\ndata: \"payload\":{\"parts\":[{\"text\":\"ok\"}]}}\n\n")
	}))
	defer srv.Close()

	c := NewPlatformGatewayClient(srv.URL, "", "t")
	ch, err := c.InvokeAgentStream(context.Background(), "ka", nil)
	if err != nil {
		t.Fatalf("InvokeAgentStream: %v", err)
	}
	var count int
	for raw := range ch {
		var evt struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(*raw, &evt); err != nil {
			t.Fatalf("multi-line data did not join into valid JSON: %q", string(*raw))
		}
		if evt.Type != "task/artifact" {
			t.Errorf("unexpected type %q", evt.Type)
		}
		count++
	}
	if count != 1 {
		t.Errorf("expected 1 joined event, got %d", count)
	}
}
