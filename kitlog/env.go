package kitlog

import (
	"log/slog"
	"os"
	"strings"
	"sync"
)

// baseLogger holds the logger installed by Init(); nil until Init() runs.
var (
	baseMu     sync.RWMutex
	baseLogger *slog.Logger
)

// lazyDefault derives the identity-seeded logger exactly once for the
// Init()-was-not-called path.
var lazyDefault = sync.OnceValue(FromEnv)

// Init is the one-line front door — the required setup path for a kit sidecar.
// It reads the Sidecar-Manager-injected environment, builds the standardized
// JSON-to-stdout logger seeded with the common identity fields, installs it as
// slog.Default(), stores it as the kitlog base (returned by Default()), and
// returns it. Calling it purely for its side effect is enough; the return value
// is a convenience handle for callers who want one:
//
//	func main() {
//		kitlog.Init()              // that's it — slog.Info(...) now carries identity
//		// ...or grab the handle:
//		logger := kitlog.Init()
//	}
//
// After Init(), plain slog.Info(...) / slog.Default() everywhere already emit
// tenant_id / kit_id / component / registration_id. No separate SetDefault call,
// no manual field wiring, no per-request code required.
//
// Init is idempotent and safe to call once at startup: a later call simply
// rebuilds and re-installs from the current environment. It never returns nil
// and never errors (kitlog is infallible by construction).
func Init() *slog.Logger {
	lg := FromEnv()
	slog.SetDefault(lg)
	baseMu.Lock()
	baseLogger = lg
	baseMu.Unlock()
	return lg
}

// Default returns the Init()-installed base logger for use as a slog.Default()
// replacement in fallback paths (e.g. the SDK config-seam nil-Logger branches,
// streampipeline's logger == nil branch). If Init() was never called (a unit
// test, or a caller that only wants a fallback), it lazily derives one from
// FromEnv() so the result is ALWAYS a non-nil, identity-seeded logger — never
// Go's plain slog.Default(). Safe to call repeatedly and concurrently.
func Default() *slog.Logger {
	baseMu.RLock()
	lg := baseLogger
	baseMu.RUnlock()
	if lg != nil {
		return lg
	}
	return lazyDefault()
}

// FromEnv builds the standard kit sidecar logger: New(...) with the level and
// format taken from the environment, seeded with the common identity fields
// (tenant_id, kit_id, component, registration_id) derived from the
// Sidecar-Manager-injected environment. A field whose source env var is unset
// is OMITTED — never logged as an empty string, never fabricated.
//
// FromEnv is the "build but do NOT install as default" variant: it returns the
// logger without touching slog.Default(). Init() is FromEnv() + slog.SetDefault
// + base storage — prefer Init() in a kit main(); reach for FromEnv() only when
// you deliberately want a handle without mutating the process default.
func FromEnv() *slog.Logger {
	return New(Options{
		Level:  LevelFromEnv(),
		Format: formatFromEnv(),
		Fields: CommonFieldsFromEnv(),
	})
}

// CommonFieldsFromEnv reads the identity env vars and returns the attrs to seed.
// Exported so a caller composing its own Options can reuse the exact derivation.
// Order is stable: tenant_id, kit_id, component, registration_id.
func CommonFieldsFromEnv() []slog.Attr {
	return commonFields(
		os.Getenv(EnvTenantID),
		os.Getenv(EnvKitID),
		os.Getenv(EnvComponent),
		os.Getenv(EnvRegistrationID),
	)
}

// commonFields is the pure core (no os access) — the unit/PBT test seam.
func commonFields(tenantID, kitID, component, registrationID string) []slog.Attr {
	attrs := make([]slog.Attr, 0, 4)
	if tenantID != "" {
		attrs = append(attrs, slog.String(KeyTenantID, tenantID))
	}
	if kitID != "" {
		attrs = append(attrs, slog.String(KeyKitID, kitID))
	}
	// component: explicit OGA_COMPONENT wins, else derive from the reg-id suffix.
	if component == "" {
		component = ComponentFromRegistrationID(registrationID)
	}
	if component != "" {
		attrs = append(attrs, slog.String(KeyComponent, component))
	}
	if registrationID != "" {
		attrs = append(attrs, slog.String(KeyRegistrationID, registrationID))
	}
	return attrs
}

// ComponentFromRegistrationID extracts the sidecar name from an
// AGENT_REGISTRATION_ID of the form "{tenant}.{name}" → "{name}". A value with
// no "." is returned unchanged; an empty value yields "".
func ComponentFromRegistrationID(regID string) string {
	regID = strings.TrimSpace(regID)
	if regID == "" {
		return ""
	}
	if i := strings.LastIndex(regID, "."); i >= 0 && i+1 < len(regID) {
		return regID[i+1:]
	}
	return regID
}
