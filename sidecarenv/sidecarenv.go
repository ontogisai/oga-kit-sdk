// Package sidecarenv names the environment variables the platform's Sidecar
// Manager sets on every kit sidecar: agents, MCP tool servers, loaders and
// source connectors. They are a contract between the platform and the SDK, so
// each is spelled once, here, and every SDK package reads it by this name.
//
// The package imports nothing, so any SDK package (and any kit) can use it.
// Variables only one package reads, such as kitlog's OGA_LOG_LEVEL, stay with
// that package.
package sidecarenv

const (
	// TenantID is the tenant slug the sidecar serves. An agent answers only
	// messages for this tenant (agent.ListenAndServe).
	TenantID = "OGA_TENANT_ID"

	// KitID is the id of the kit the sidecar belongs to.
	KitID = "OGA_KIT_ID"

	// Component is the sidecar's own name.
	Component = "OGA_COMPONENT"

	// RegistrationID is the sidecar's registration id, {tenant}.{name}: the
	// identity its workload token is minted for.
	RegistrationID = "AGENT_REGISTRATION_ID"

	// GatewayURL is the Platform Access Gateway's base URL.
	GatewayURL = "PLATFORM_GATEWAY_URL"

	// BootstrapKind names the identity the sidecar mints its workload token
	// from: "k8s_sa" (SATokenPath) or "secret" (BootstrapSecret).
	BootstrapKind = "AGENT_BOOTSTRAP_KIND"

	// SATokenPath is the projected ServiceAccount token file, set with
	// BootstrapKind "k8s_sa".
	SATokenPath = "AGENT_SA_TOKEN_PATH"

	// BootstrapSecret is the bootstrap secret, set with BootstrapKind
	// "secret".
	BootstrapSecret = "AGENT_BOOTSTRAP_SECRET" //nolint:gosec // G101: the variable's name, not a credential
)
