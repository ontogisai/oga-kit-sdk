package kitlog

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format selects the handler encoding.
type Format string

const (
	FormatJSON Format = "json" // production / platform parity (default)
	FormatText Format = "text" // local `go run` ergonomics only
)

// Options tunes New. The zero value is valid and yields the platform baseline:
// a JSON handler writing to os.Stdout at Info level.
type Options struct {
	// Level is the minimum level. When nil, LevelFromEnv() is used.
	Level slog.Leveler
	// Format selects JSON (default) or text. Empty → FormatJSON.
	Format Format
	// Writer is the sink. Nil → os.Stdout (platform parity). Tests point this
	// at a *bytes.Buffer to assert emitted records.
	Writer io.Writer
	// AddSource adds source file:line (slog HandlerOptions.AddSource). Default false.
	AddSource bool
	// Fields are common attributes pre-applied via logger.With, in order.
	// Callers rarely set this directly — FromEnv derives it from the environment.
	Fields []slog.Attr
}

// New builds a *slog.Logger with a handler matching the oga-platform baseline:
//
//	slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: LevelFromEnv()}))
//
// then applies Options.Fields via With. The handler installs an error-key
// canonicalizer (see errorKeyCanonicalizer). It never returns nil and never
// errors.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stdout // platform parity: stdout, not stderr
	}
	level := opts.Level
	if level == nil {
		level = LevelFromEnv()
	}
	ho := &slog.HandlerOptions{
		Level:     level,
		AddSource: opts.AddSource,
		// errorKeyCanonicalizer rewrites any error-typed attribute to the
		// canonical "error" key, so authors never have to name KeyError. v1
		// kitlog OWNS ReplaceAttr — a caller-supplied ReplaceAttr is not part
		// of the Options surface (kept minimal; see design §LLD-7).
		ReplaceAttr: errorKeyCanonicalizer,
	}

	var h slog.Handler
	switch opts.Format {
	case FormatText:
		h = slog.NewTextHandler(w, ho)
	default: // FormatJSON and "" both → JSON
		h = slog.NewJSONHandler(w, ho)
	}
	logger := slog.New(h)
	if len(opts.Fields) > 0 {
		args := make([]any, 0, len(opts.Fields))
		for _, a := range opts.Fields {
			args = append(args, a)
		}
		logger = logger.With(args...)
	}
	return logger
}

// LevelFromEnv parses OGA_LOG_LEVEL (debug|info|warn|error, case-insensitive).
// Unset or unrecognized → slog.LevelInfo. Mirrors the platform logLevelFromEnv().
func LevelFromEnv() slog.Level { return ParseLevel(os.Getenv(EnvLogLevel)) }

// ParseLevel maps a string to a slog.Level. Unknown → LevelInfo (never errors).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// formatFromEnv resolves OGA_LOG_FORMAT (json|text). Unset/unknown → FormatJSON.
func formatFromEnv() Format {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvLogFormat)), string(FormatText)) {
		return FormatText
	}
	return FormatJSON
}

// Err returns an attr carrying err under the canonical KeyError ("error"), so a
// kit author never has to remember the key string:
//
//	slog.Error("load failed", kitlog.Err(err))
//
// It is nil-safe: Err(nil) returns the zero slog.Attr{}, which slog OMITS from
// the record (no "error":"<nil>" spam). This is the ergonomic explicit path; it
// composes with — and is idempotent under — the handler canonicalizer (an attr
// already keyed "error" passes through unchanged).
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{} // elided by slog
	}
	return slog.Any(KeyError, err)
}

// errorKeyCanonicalizer is the slog.HandlerOptions.ReplaceAttr installed by New.
// It rewrites the key of any NON-built-in, error-typed attribute to KeyError, so
// slog.Error("msg", "err", err) / ("error", err) / (kitlog.Err(err)) ALL emit
// the canonical "error" key regardless of what the author typed.
//
// Pass-through rules (unchanged attrs):
//   - built-in top-level attrs (time/level/msg) — the empty group path plus
//     slog's reserved keys — are never rewritten;
//   - an attr already keyed KeyError is left as-is (idempotent);
//   - a non-error-valued attr is left as-is.
//
// Detection: an error value arrives as slog.KindAny; we type-assert Any().(error).
// Caveat: if a record carries TWO error-typed attrs they BOTH become "error"
// (a rare authoring mistake — documented known edge, not a defect).
func errorKeyCanonicalizer(groups []string, a slog.Attr) slog.Attr {
	// Only touch top-level attrs; never rewrite inside a group, and never the
	// built-in time/level/msg (those are top-level with reserved keys).
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey, slog.LevelKey, slog.MessageKey, KeyError:
			return a // built-ins + already-canonical pass through
		}
	}
	if a.Value.Kind() == slog.KindAny {
		if _, ok := a.Value.Any().(error); ok {
			a.Key = KeyError
		}
	}
	return a
}
