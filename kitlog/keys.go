package kitlog

import "github.com/ontogisai/oga-kit-sdk/sidecarenv"

// Field keys — the ONE spelling for every structured field a kit log carries.
// Referencing these constants (never a string literal) is what prevents field
// drift across SDK packages and across the SDK↔platform boundary.
const (
	KeyTenantID       = "tenant_id"       // tenant slug, e.g. "tnt1" (OGA-289 canonical key)
	KeyKitID          = "kit_id"          // domain kit id
	KeyComponent      = "component"       // this sidecar's own name (agent/mcp/loader/connector)
	KeyRegistrationID = "registration_id" // AGENT_REGISTRATION_ID, "{tenant}.{name}"
	KeyService        = "service"         // platform-parity alias for component (opt-in)

	KeyTraceID = "trace_id" // per-request correlation id
	KeySpanID  = "span_id"  // per-request span id
	KeyTaskID  = "task_id"  // A2A / pipeline task id

	// KeyError is the canonical error field. Attach an error via Err(err) — or
	// under any key; the handler canonicalizes any error-typed attr to "error".
	KeyError = "error"
)

// The sidecar identity variables kitlog reads. Their names are in sidecarenv,
// the one place every SDK package takes them from.
const (
	// Deprecated: use sidecarenv.TenantID.
	EnvTenantID = sidecarenv.TenantID
	// Deprecated: use sidecarenv.RegistrationID.
	EnvRegistrationID = sidecarenv.RegistrationID
	// Deprecated: use sidecarenv.KitID.
	EnvKitID = sidecarenv.KitID
	// Deprecated: use sidecarenv.Component.
	EnvComponent = sidecarenv.Component
)

// The logging variables, which only kitlog reads.
const (
	EnvLogLevel  = "OGA_LOG_LEVEL"  // debug|info|warn|error (default info)
	EnvLogFormat = "OGA_LOG_FORMAT" // json|text (default json)
)
