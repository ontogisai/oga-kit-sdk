package streampipeline

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/internal/redact"
)

// OGA-1134: the ReAct log's terminal status line quotes the status error's
// message, and what fills that message is not checked (an upstream error can
// quote a response body). The line quotes at most redact.MaxQuotedBody bytes
// of it, like any body quoted in an error.
func TestLogDrainedEvent_StatusErrorIsCapped(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	msg := strings.Repeat("x", 4*redact.MaxQuotedBody)
	logDrainedEvent(t.Context(), logger, Input{Actor: agent.EventActor{ID: "a"}}, &agent.StreamEvent{
		Type: agent.EventTypeStatus,
		Payload: &agent.StatusPayload{
			State: agent.TaskStateFailed,
			Error: &agent.StatusError{Code: -32000, Message: msg},
		},
	}, DefaultConfig())

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	got, _ := line["error"].(string)
	if strings.Contains(got, strings.Repeat("x", redact.MaxQuotedBody+1)) {
		t.Errorf("status error quotes more than %d bytes of the message (%d bytes)", redact.MaxQuotedBody, len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", redact.MaxQuotedBody)) || !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("error = %.80q…, want the first %d bytes and the truncation marker", got, redact.MaxQuotedBody)
	}
}

// A message within the cap is logged as it is.
func TestLogDrainedEvent_ShortStatusErrorIsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logDrainedEvent(t.Context(), logger, Input{Actor: agent.EventActor{ID: "a"}}, &agent.StreamEvent{
		Type: agent.EventTypeStatus,
		Payload: &agent.StatusPayload{
			State: agent.TaskStateFailed,
			Error: &agent.StatusError{Code: -32000, Message: "tool kg_get_entity failed"},
		},
	}, DefaultConfig())

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	if line["error"] != "tool kg_get_entity failed" {
		t.Errorf("error = %v, want the message unchanged", line["error"])
	}
}
