package kitlog

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestInto_From_RoundTrip(t *testing.T) {
	lg := New(Options{Writer: &bytes.Buffer{}})
	ctx := Into(context.Background(), lg)
	if From(ctx) != lg {
		t.Fatal("From(Into(ctx, lg)) must return lg")
	}
}

func TestInto_NilLogger_NoOp(t *testing.T) {
	ctx := context.Background()
	if Into(ctx, nil) != ctx {
		t.Fatal("Into(ctx, nil) must return ctx unchanged")
	}
}

func TestFrom_NoLogger_DefaultsNonNil(t *testing.T) {
	if From(context.Background()) == nil {
		t.Fatal("From with no stored logger must return a non-nil default")
	}
	//nolint:staticcheck // deliberately exercising the nil-ctx guard
	if From(nil) == nil {
		t.Fatal("From(nil) must return a non-nil default")
	}
}

func TestWithRequest_Additive(t *testing.T) {
	var buf bytes.Buffer
	base := New(Options{Writer: &buf, Fields: commonFields("sgac1", "k", "c", "sgac1.c")})
	lg := WithRequest(base, RequestFields{TraceID: "trace-1", TaskID: "task-1"})
	lg.Info("handling")
	m := parseOneJSON(t, buf.Bytes())
	// base identity preserved
	if m[KeyTenantID] != "sgac1" {
		t.Fatalf("base identity lost: %v", m)
	}
	// per-request added
	if m[KeyTraceID] != "trace-1" || m[KeyTaskID] != "task-1" {
		t.Fatalf("per-request fields missing: %v", m)
	}
	// empty span omitted
	if _, ok := m[KeySpanID]; ok {
		t.Fatalf("empty span_id should be omitted: %v", m)
	}
}

func TestWithRequest_AllEmpty_ReturnsBase(t *testing.T) {
	base := New(Options{Writer: &bytes.Buffer{}})
	if got := WithRequest(base, RequestFields{}); got != base {
		t.Fatal("WithRequest with all-empty fields should return the base logger")
	}
}

func TestWithRequest_NilBase(t *testing.T) {
	if WithRequest(nil, RequestFields{TraceID: "x"}) == nil {
		t.Fatal("WithRequest(nil, ...) must not return nil")
	}
}

func TestWithRequest_Extra(t *testing.T) {
	var buf bytes.Buffer
	base := New(Options{Writer: &buf})
	lg := WithRequest(base, RequestFields{Extra: []slog.Attr{slog.String("custom", "v")}})
	lg.Info("x")
	m := parseOneJSON(t, buf.Bytes())
	if m["custom"] != "v" {
		t.Fatalf("extra attr missing: %v", m)
	}
}
