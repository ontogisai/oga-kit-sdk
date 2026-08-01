package kitlog

import (
	"context"
	"log/slog"
)

// This file is OPTIONAL / secondary. Everything here is opt-in enrichment for
// authors who want request-scoped trace_id / span_id / task_id. The required
// setup path is Init() alone — the base logger already produces fully-identified
// logs without any of these helpers.

// ctxKey is the unexported context key for the stored logger (avoids collisions).
type ctxKey struct{}

// Into returns a child context carrying lg. A nil lg is ignored (ctx returned
// unchanged) so a caller can never poison the context with a nil logger.
func Into(ctx context.Context, lg *slog.Logger) context.Context {
	if lg == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, lg)
}

// From returns the logger stored by Into, or slog.Default() when none is
// present. It never returns nil.
func From(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if lg, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && lg != nil {
			return lg
		}
	}
	return slog.Default()
}

// RequestFields are the per-request correlation values folded onto a logger by
// WithRequest. Empty fields are skipped (not logged as "").
type RequestFields struct {
	TraceID string
	SpanID  string
	TaskID  string
	// Extra carries any additional per-request attrs (already keyed via the
	// Key* constants by the caller). Optional.
	Extra []slog.Attr
}

// WithRequest returns a child logger enriched with the non-empty request fields.
// The base logger already carries the common identity fields (via FromEnv), so
// the result carries identity + per-request correlation in every line.
func WithRequest(lg *slog.Logger, rf RequestFields) *slog.Logger {
	if lg == nil {
		lg = slog.Default()
	}
	attrs := make([]slog.Attr, 0, 3+len(rf.Extra))
	if rf.TraceID != "" {
		attrs = append(attrs, slog.String(KeyTraceID, rf.TraceID))
	}
	if rf.SpanID != "" {
		attrs = append(attrs, slog.String(KeySpanID, rf.SpanID))
	}
	if rf.TaskID != "" {
		attrs = append(attrs, slog.String(KeyTaskID, rf.TaskID))
	}
	attrs = append(attrs, rf.Extra...)
	if len(attrs) == 0 {
		return lg
	}
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a)
	}
	return lg.With(args...)
}

// WithRequestInto is the common composition: enrich the context's logger with
// request fields and store the result back on the context in one call.
func WithRequestInto(ctx context.Context, rf RequestFields) context.Context {
	return Into(ctx, WithRequest(From(ctx), rf))
}
