package kitlog

// Field keys — the ONE spelling for every structured field a kit log carries.
// Referencing these constants (never a string literal) is what prevents field
// drift across SDK packages and across the SDK↔platform boundary.
const (
	KeyTenantID       = "tenant_id"       // tenant slug, e.g. "sgac1" (OGA-289 canonical key)
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

// Environment variable names kitlog reads. The identity vars reuse the exact
// names the rest of the SDK already reads (gateway/client_env.go,
// auth/token_manager_env.go); the OGA_LOG_* vars are new and logging-specific.
const (
	EnvTenantID       = "OGA_TENANT_ID"         // existing (gateway.NewClientFromEnv)
	EnvRegistrationID = "AGENT_REGISTRATION_ID" // existing (auth.NewTokenManagerFromEnv)
	EnvKitID          = "OGA_KIT_ID"            // sidecar-manager to inject
	EnvComponent      = "OGA_COMPONENT"         // sidecar-manager to inject (optional)
	EnvLogLevel       = "OGA_LOG_LEVEL"         // debug|info|warn|error (default info)
	EnvLogFormat      = "OGA_LOG_FORMAT"        // json|text (default json)
)
