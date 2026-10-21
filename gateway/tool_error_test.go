package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/internal/redact"
)

// answering returns a gateway stub that answers every request with status and
// body.
func answering(t *testing.T, status int, body []byte) *PlatformGatewayClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return NewPlatformGatewayClient(srv.URL, "", "acme")
}

// toolErrorBody is the structured tool error the Platform Gateway answers
// with, its message message bytes long.
func toolErrorBody(t *testing.T, message string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"code":        "OGA-MCPG-DEPS-4003",
		"message":     message,
		"category":    "DEPS",
		"service":     "MCPG",
		"details":     map[string]any{"tool_name": "fm_raise_incident"},
		"retry":       false,
		"http_status": 424,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A structured error longer than the 512 bytes the error text quotes still
// reaches the caller as a ToolError (OGA-1136). Before, CallTool parsed the
// capped text and returned a plain error.
func TestCallTool_LongStructuredErrorIsAToolError(t *testing.T) {
	message := "kit tool fm_raise_incident reported an error: " + strings.Repeat("x", 4<<10)
	body := toolErrorBody(t, message)
	if len(body) <= redact.MaxQuotedBody {
		t.Fatalf("body is %d bytes; the test needs more than %d", len(body), redact.MaxQuotedBody)
	}

	_, err := answering(t, http.StatusFailedDependency, body).CallTool(t.Context(), "fm_raise_incident", map[string]any{})
	toolErr, ok := errors.AsType[*ToolError](err)
	if !ok {
		t.Fatalf("CallTool error = %v (%T), want a *ToolError", err, err)
	}
	want := &ToolError{
		Code: "OGA-MCPG-DEPS-4003", Message: message, Category: "DEPS", Service: "MCPG",
		Details:    map[string]any{"tool_name": "fm_raise_incident"},
		HTTPStatus: http.StatusFailedDependency, ToolName: "fm_raise_incident",
	}
	if !reflect.DeepEqual(toolErr, want) {
		t.Errorf("ToolError = %+v, want %+v", toolErr, want)
	}
}

// A short structured error decodes exactly as the text parse decoded it.
func TestCallTool_ShortStructuredErrorUnchanged(t *testing.T) {
	body := toolErrorBody(t, "kit tool fm_raise_incident reported an error: priority not accepted")
	if len(body) > redact.MaxQuotedBody {
		t.Fatalf("body is %d bytes; the test needs at most %d", len(body), redact.MaxQuotedBody)
	}

	_, err := answering(t, http.StatusFailedDependency, body).CallTool(t.Context(), "fm_raise_incident", map[string]any{})
	got, ok := errors.AsType[*ToolError](err)
	if !ok {
		t.Fatalf("CallTool error = %v (%T), want a *ToolError", err, err)
	}
	// The text form the client produced before OGA-1136, parsed the old way.
	want := ParseToolError(fmt.Errorf("gateway error %d: %s", http.StatusFailedDependency, body))
	if want == nil {
		t.Fatal("the text form did not parse")
	}
	want.ToolName = "fm_raise_incident"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToolError = %+v, want %+v", got, want)
	}
	if got.Error() != "tool fm_raise_incident: [OGA-MCPG-DEPS-4003] kit tool fm_raise_incident reported an error: priority not accepted" {
		t.Errorf("Error() = %q", got.Error())
	}
}

// An answer that is not a structured error stays a plain error, and its text
// quotes at most 512 bytes of the body.
func TestCallTool_UnstructuredErrorTextIsCapped(t *testing.T) {
	_, err := answering(t, http.StatusBadGateway, []byte(strings.Repeat("y", 4<<10))).CallTool(t.Context(), "fm_status", nil)
	if _, ok := errors.AsType[*ToolError](err); ok || err == nil {
		t.Fatalf("CallTool error = %v, want a plain error", err)
	}
	prefix := "call tool fm_status: gateway error 502: "
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("error = %q, want prefix %q", err.Error(), prefix)
	}
	if quoted := strings.TrimSuffix(strings.TrimPrefix(err.Error(), prefix), "…(truncated)"); len(quoted) > redact.MaxQuotedBody {
		t.Errorf("error quotes %d bytes of the body, want at most %d", len(quoted), redact.MaxQuotedBody)
	}
}

// The error text of a ToolError quotes at most 512 bytes of the platform's
// answer: 448 of the message and 64 of the code.
func TestToolError_ErrorIsBounded(t *testing.T) {
	e := &ToolError{Code: strings.Repeat("C", 1<<10), Message: strings.Repeat("m", 4<<10), ToolName: "fm_raise_incident"}
	text := e.Error()
	code := strings.Repeat("C", maxQuotedCode)
	msg := strings.Repeat("m", maxQuotedMessage)
	if want := "tool fm_raise_incident: [" + code + "…(truncated)] " + msg + "…(truncated)"; text != want {
		t.Errorf("Error() = %d bytes, want %d", len(text), len(want))
	}
	if maxQuotedCode+maxQuotedMessage != redact.MaxQuotedBody {
		t.Errorf("code %d + message %d bytes, want %d together", maxQuotedCode, maxQuotedMessage, redact.MaxQuotedBody)
	}
}

// An error answer is read up to 64 KiB, however long it is.
func TestPost_ErrorBodyReadIsBounded(t *testing.T) {
	_, err := answering(t, http.StatusInternalServerError, []byte(strings.Repeat("z", 200<<10))).post(t.Context(), "/mcp/tools/call", nil)
	gerr, ok := errors.AsType[*gatewayHTTPError](err)
	if !ok {
		t.Fatalf("post error = %v (%T), want a *gatewayHTTPError", err, err)
	}
	if len(gerr.body) != maxErrorBodyRead {
		t.Errorf("read %d bytes of the error answer, want %d", len(gerr.body), maxErrorBodyRead)
	}
}

// ParseToolError keeps parsing the text form, for an error it did not produce.
func TestParseToolError_TextForm(t *testing.T) {
	got := ParseToolError(errors.New(`gateway error 403: {"code":"OGA-CORE-DENY-1202","message":"denied","category":"DENY","http_status":403}`))
	if got == nil || got.Code != "OGA-CORE-DENY-1202" || !got.IsPermissionError() || got.HTTPStatus != http.StatusForbidden {
		t.Errorf("ParseToolError = %+v", got)
	}
	if ParseToolError(errors.New("gateway error 502: upstream down")) != nil {
		t.Error("an unstructured text parsed as a tool error")
	}
}
