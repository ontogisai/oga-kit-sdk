package auth

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// WorkloadTokenProviderFromEnv builds a workload-token provider for a sidecar
// that calls the Platform Gateway without the agent runtime
// (agent.ConnectRuntimeDeps): a loader or a source connector on
// transfer.HTTPCommitClient, a time-series sink, or any other direct caller.
//
// When the Sidecar Manager has injected a bootstrap identity
// (AGENT_BOOTSTRAP_KIND set: "k8s_sa" on Kubernetes, "secret" elsewhere), it
// mints a platform workload token at {gatewayURL}/auth/token/issue and renews
// it in memory at 50% of its lifetime, returning:
//   - provider: a func() string yielding the current bearer token, suitable for
//     transfer.WithTokenProvider or any per-request token hook;
//   - stop: a func() that stops the renewal loop (call on shutdown);
//   - err: non-nil when a bootstrap identity IS configured but the first mint
//     failed. The caller MUST treat this as fatal (the sidecar cannot prove
//     its identity and must not start serving), never run tokenless.
//
// When no bootstrap identity is configured (AGENT_BOOTSTRAP_KIND unset, for
// example a sidecar run outside the platform), it returns (nil, noop, nil).
// The caller then sends no Authorization header, and the Platform Gateway
// refuses its requests with 401: the gateway has no tokenless mode.
//
// AGENT_REGISTRATION_ID ("{tenant}.{name}", manager-injected) is the identity
// the mint endpoint authorizes the bootstrap credential against.
//
// Renewal runs until stop is called or ctx ends: pass a context that lives as
// long as the sidecar.
func WorkloadTokenProviderFromEnv(ctx context.Context, gatewayURL string) (provider func() string, stop func(), err error) {
	noop := func() {}

	bp, err := BootstrapFromEnv()
	if err != nil {
		return nil, noop, fmt.Errorf("resolve bootstrap identity: %w", err)
	}
	if bp == nil {
		// No bootstrap identity injected: no token.
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
