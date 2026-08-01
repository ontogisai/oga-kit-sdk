package kitlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// parseOneJSON parses a single-record buffer into a map. Fails the test on
// invalid JSON. Shared test helper.
func parseOneJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	b = bytes.TrimSpace(b)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON record %q: %v", b, err)
	}
	return m
}

func TestNew_ZeroOptions_JSONInfo(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Writer: &buf})
	lg.Info("hello", "k", "v")
	lg.Debug("suppressed") // below Info default

	m := parseOneJSON(t, buf.Bytes())
	if m[slog.MessageKey] != "hello" {
		t.Fatalf("msg = %v, want hello", m[slog.MessageKey])
	}
	if m[slog.LevelKey] != "INFO" {
		t.Fatalf("level = %v, want INFO", m[slog.LevelKey])
	}
	if _, ok := m[slog.TimeKey]; !ok {
		t.Fatalf("missing time")
	}
	if strings.Contains(buf.String(), "suppressed") {
		t.Fatalf("debug record should be filtered at Info level")
	}
}

func TestNew_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Writer: &buf, Format: FormatText})
	lg.Info("hi")
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Fatalf("text handler should not emit JSON: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "msg=hi") {
		t.Fatalf("text output missing msg=hi: %s", buf.String())
	}
}

func TestParseLevel_Table(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	// surrounding whitespace is trimmed
	if got := ParseLevel("  warn "); got != slog.LevelWarn {
		t.Errorf("ParseLevel with surrounding whitespace = %v, want Warn", got)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Writer: &buf, Level: slog.LevelWarn})
	lg.Info("info-suppressed")
	lg.Warn("warn-kept")
	lg.Error("error-kept")
	out := buf.String()
	if strings.Contains(out, "info-suppressed") {
		t.Fatalf("Info should be filtered at Warn level: %s", out)
	}
	if !strings.Contains(out, "warn-kept") || !strings.Contains(out, "error-kept") {
		t.Fatalf("Warn/Error should be kept at Warn level: %s", out)
	}
}

func TestErr_NilSafe(t *testing.T) {
	if got := Err(nil); !got.Equal(slog.Attr{}) {
		t.Fatalf("Err(nil) = %#v, want zero slog.Attr{}", got)
	}
	if got := Err(errors.New("boom")); got.Key != KeyError {
		t.Fatalf("Err(err).Key = %q, want %q", got.Key, KeyError)
	}
}

func TestErr_EmitsNoFieldForNil(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Writer: &buf}).Error("m", Err(nil))
	m := parseOneJSON(t, buf.Bytes())
	if _, ok := m[KeyError]; ok {
		t.Fatalf("Err(nil) must emit no error field, got: %v", m[KeyError])
	}
}

func TestErrorKeyCanonicalizer(t *testing.T) {
	cases := []string{"err", "cause", "error"}
	for _, key := range cases {
		var buf bytes.Buffer
		lg := New(Options{Writer: &buf})
		lg.Error("boom", key, errors.New("the-error"))
		m := parseOneJSON(t, buf.Bytes())
		if m[KeyError] != "the-error" {
			t.Errorf("key %q: canonical error = %v, want the-error", key, m[KeyError])
		}
		if key != KeyError {
			if _, ok := m[key]; ok {
				t.Errorf("key %q: original key should be gone after canonicalization", key)
			}
		}
	}
}

func TestErrorKeyCanonicalizer_LeavesNonErrorAndBuiltins(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Writer: &buf})
	lg.Info("msg-here", "count", 5, "name", "widget")
	m := parseOneJSON(t, buf.Bytes())
	if m["count"] != float64(5) {
		t.Fatalf("non-error field count mangled: %v", m["count"])
	}
	if m["name"] != "widget" {
		t.Fatalf("non-error field name mangled: %v", m["name"])
	}
	if m[slog.MessageKey] != "msg-here" {
		t.Fatalf("built-in msg mangled: %v", m[slog.MessageKey])
	}
}

func TestComponentFromRegistrationID(t *testing.T) {
	cases := map[string]string{
		"sgac1.fm-operations-agent": "fm-operations-agent",
		"plainname":                 "plainname",
		"":                          "",
		"  sgac1.x  ":               "x",
		"a.b.c":                     "c",
	}
	for in, want := range cases {
		if got := ComponentFromRegistrationID(in); got != want {
			t.Errorf("ComponentFromRegistrationID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommonFields_OmitsEmpty(t *testing.T) {
	attrs := commonFields("", "", "", "")
	if len(attrs) != 0 {
		t.Fatalf("all-empty commonFields should yield no attrs, got %d", len(attrs))
	}
	attrs = commonFields("t", "", "", "t.comp")
	// tenant_id + component (derived from reg-id) + registration_id = 3
	keys := map[string]bool{}
	for _, a := range attrs {
		keys[a.Key] = true
	}
	if keys[KeyKitID] {
		t.Fatalf("kit_id should be omitted when empty")
	}
	for _, want := range []string{KeyTenantID, KeyComponent, KeyRegistrationID} {
		if !keys[want] {
			t.Fatalf("expected %q to be present", want)
		}
	}
}

func TestCommonFields_ExplicitComponentWins(t *testing.T) {
	attrs := commonFields("t", "k", "explicit", "t.derived")
	var comp string
	for _, a := range attrs {
		if a.Key == KeyComponent {
			comp = a.Value.String()
		}
	}
	if comp != "explicit" {
		t.Fatalf("explicit component should win, got %q", comp)
	}
}
