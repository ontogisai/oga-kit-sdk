package auth

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// WorkloadTokenProviderFromEnv builds a workload-token provider for a non-agent
// sidecar (data/ontology loaders, MCP servers) that calls the Platform Gateway
// directly — i.e. a sidecar that does NOT go through the agent chassis
// (agent.ConnectRuntimeDeps) but still needs to authenticate to the gateway
// under OGA-404.
//
// When the Sidecar Manager has injected a bootstrap identity
// (AGENT_BOOTSTRAP_KIND set — "secret" off-cluster, "k8s_sa" in-cluster), it
// mints a platform RS256 workload token at {gatewayURL}/auth/token/issue and
// rotates it in memory (sliding renewal, no token file), returning:
//   - provider: a func() string yielding the current bearer token, suitable for
//     transfer.WithTokenProvider or any per-request token hook;
//   - stop: a func() that stops the renewal loop (call on shutdown);
//   - err: non-nil when a bootstrap identity IS configured but the initial mint
//     failed — the caller MUST treat this as fatal (the sidecar cannot prove
//     identity and must not start serving), never silently run tokenless.
//
// When no bootstrap identity is configured (AGENT_BOOTSTRAP_KIND unset), it
// returns (nil, noop, nil) so the caller can fall back to a legacy token file
// (transfer.WithTokenPath) or, in a dev-mode gateway, no Authorization header.
//
// AGENT_REGISTRATION_ID ("{tenant}.{name}", manager-injected) is the identity
// the mint endpoint authorizes the bootstrap credential against.
func WorkloadTokenProviderFromEnv(ctx context.Context, gatewayURL string) (provider func() string, stop func(), err error) {
	noop := func() {}

	bp, err := BootstrapFromEnv()
	if err != nil {
		return nil, noop, fmt.Errorf("resolve bootstrap identity: %w", err)
	}
	if bp == nil {
		// No bootstrap identity injected — caller falls back to token file / dev.
		return nil, noop, nil
	}

	tm, err := NewTokenManager(ctx, &TokenManagerConfig{
		Bootstrap:           bp,
		IssueURL:            strings.TrimRight(gatewayURL, "/") + "/auth/token/issue",
		AgentRegistrationID: os.Getenv("AGENT_REGISTRATION_ID"),
	})
	if err != nil {
		return nil, noop, fmt.Errorf("mint workload token: %w", err)
	}
	return tm.Token, tm.Stop, nil
}
