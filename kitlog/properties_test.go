package kitlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"pgregory.net/rapid"
)

// Property 1: Field-key stability — the exported constants have fixed spellings.
func TestFieldKeyStability(t *testing.T) {
	want := map[string]string{
		KeyTenantID:       "tenant_id",
		KeyKitID:          "kit_id",
		KeyComponent:      "component",
		KeyRegistrationID: "registration_id",
		KeyService:        "service",
		KeyTraceID:        "trace_id",
		KeySpanID:         "span_id",
		KeyTaskID:         "task_id",
		KeyError:          "error",
	}
	for k, v := range want {
		if k != v {
			t.Errorf("field-key constant drift: %q != %q", k, v)
		}
	}
}

// Property 2 + 3: env-seeded logger carries every non-empty common field, and
// omits empty ones.
func TestProp_CommonFieldsPresentAndEmptyOmitted(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		tid := rapid.StringMatching(`[a-z0-9]{1,12}`).Draw(rt, "tenant")
		kid := rapid.StringMatching(`[a-z0-9-]{1,20}`).Draw(rt, "kit")
		comp := rapid.StringMatching(`[a-z0-9-]{1,20}`).Draw(rt, "component")
		reg := tid + "." + comp

		var buf bytes.Buffer
		lg := New(Options{Writer: &buf, Fields: commonFields(tid, kid, comp, reg)})
		lg.Info("x")
		m := parseRecord(rt, buf.Bytes())
		requireEq(rt, m[KeyTenantID], tid)
		requireEq(rt, m[KeyKitID], kid)
		requireEq(rt, m[KeyComponent], comp)
		requireEq(rt, m[KeyRegistrationID], reg)

		// all-empty ⇒ none present
		var buf2 bytes.Buffer
		New(Options{Writer: &buf2, Fields: commonFields("", "", "", "")}).Info("x")
		m2 := parseRecord(rt, buf2.Bytes())
		for _, k := range []string{KeyTenantID, KeyKitID, KeyComponent, KeyRegistrationID} {
			if _, ok := m2[k]; ok {
				rt.Fatalf("empty field %q must be omitted", k)
			}
		}
	})
}

// Property 4: ParseLevel is total (never panics) and canonical values map right.
func TestProp_ParseLevelTotal(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		s := rapid.String().Draw(rt, "s")
		got := ParseLevel(s)
		switch got {
		case slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError:
		default:
			rt.Fatalf("ParseLevel(%q) returned out-of-range level %v", s, got)
		}
	})
}

// Property 6: JSON output is valid and contains the built-ins + seeded fields.
func TestProp_ValidJSON(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		msg := rapid.String().Draw(rt, "msg")
		var buf bytes.Buffer
		lg := New(Options{Writer: &buf, Fields: commonFields("sgac1", "k", "c", "sgac1.c")})
		lg.Info(msg)
		if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
			rt.Fatalf("invalid JSON: %s", buf.Bytes())
		}
		m := parseRecord(rt, buf.Bytes())
		for _, k := range []string{slog.TimeKey, slog.LevelKey, slog.MessageKey, KeyTenantID, KeyKitID, KeyComponent, KeyRegistrationID} {
			if _, ok := m[k]; !ok {
				rt.Fatalf("missing %q in %v", k, m)
			}
		}
	})
}

// Property 5: context get-after-put returns the same logger.
func TestProp_ContextRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		lg := New(Options{Writer: &bytes.Buffer{}})
		if From(Into(context.Background(), lg)) != lg {
			rt.Fatalf("get-after-put mismatch")
		}
	})
}

// Property 14: an error under any non-reserved key canonicalizes to "error".
func TestProp_ErrorKeyCanonicalized(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		key := rapid.StringMatching(`[a-z_]{1,12}`).
			Filter(func(s string) bool {
				return s != slog.TimeKey && s != slog.LevelKey && s != slog.MessageKey
			}).
			Draw(rt, "key")
		msg := rapid.StringMatching(`[a-zA-Z0-9 ]{1,30}`).Draw(rt, "errmsg")

		var buf bytes.Buffer
		New(Options{Writer: &buf}).Error("boom", key, errors.New(msg))
		m := parseRecord(rt, buf.Bytes())
		requireEq(rt, m[KeyError], msg)
		if key != KeyError {
			if _, ok := m[key]; ok {
				rt.Fatalf("original key %q should be gone after canonicalization", key)
			}
		}
	})
}

// Property 15: Err(err) keys on "error"; Err(nil) emits no field.
func TestProp_ErrHelper(t *testing.T) {
	if got := Err(nil); !got.Equal(slog.Attr{}) {
		t.Fatalf("Err(nil) must be the zero attr")
	}
	rapid.Check(t, func(rt *rapid.T) {
		msg := rapid.StringMatching(`[a-zA-Z0-9 ]{1,30}`).Draw(rt, "errmsg")
		a := Err(errors.New(msg))
		requireEq(rt, a.Key, KeyError)
		var buf bytes.Buffer
		New(Options{Writer: &buf}).Error("m", a)
		m := parseRecord(rt, buf.Bytes())
		requireEq(rt, m[KeyError], msg)
	})
}

// --- rapid helpers ---

func parseRecord(rt *rapid.T, b []byte) map[string]any {
	rt.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &m); err != nil {
		rt.Fatalf("invalid JSON %q: %v", b, err)
	}
	return m
}

func requireEq[T comparable](rt *rapid.T, got any, want T) {
	rt.Helper()
	g, ok := got.(T)
	if !ok || g != want {
		rt.Fatalf("got %v (%T), want %v", got, got, want)
	}
}
