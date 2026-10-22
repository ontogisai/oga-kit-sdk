// Package auth gives a kit sidecar its platform workload token.
//
// The platform's Sidecar Manager gives each sidecar a bootstrap identity: on
// Kubernetes a projected ServiceAccount token (AGENT_BOOTSTRAP_KIND=k8s_sa,
// AGENT_SA_TOKEN_PATH), elsewhere a bootstrap secret
// (AGENT_BOOTSTRAP_KIND=secret, AGENT_BOOTSTRAP_SECRET). A [TokenManager]
// presents that identity to the Platform Gateway's mint endpoint
// (POST /auth/token/issue) together with the sidecar's registration id
// (AGENT_REGISTRATION_ID) and receives a short-lived workload token. It mints
// again at 50% of each token's lifetime, re-reading the bootstrap identity
// each time, and retries a failed mint with exponential backoff. The token is
// held in memory only.
//
// Renewal runs until the manager is stopped or the context passed to it ends.
// Pass a context that lives as long as the sidecar: when it ends, the manager
// logs "token renewal stopped: context done" and the token is not renewed
// again.
//
// Most kits do not use this package directly: agent.ConnectRuntimeDeps,
// mcptools.NewRuntimeFromEnv and gateway.NewClientFromEnv wire it from the
// environment. A loader, a source connector or a time-series sink uses
// [WorkloadTokenProviderFromEnv]. The SDK README, "Authenticating to the
// platform", describes the whole contract.
package auth
