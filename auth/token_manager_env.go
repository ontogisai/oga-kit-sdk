package auth

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/kitlog"
)

// EnvTokenManagerConfig configures NewTokenManagerFromEnv.
type EnvTokenManagerConfig struct {
	// GatewayURL is the Platform Gateway base URL (PLATFORM_GATEWAY_URL).
	// Required: the mint endpoint, GatewayURL + "/auth/token/issue", is derived
	// from it.
	GatewayURL string

	// AgentRegistrationID is the "{tenant}.{name}" identity the mint endpoint
	// authorizes the bootstrap credential against. Used only as a fallback when
	// the AGENT_REGISTRATION_ID environment variable (injected by the Sidecar
	// Manager) is unset.
	AgentRegistrationID string
}

// NewTokenManagerFromEnv builds a TokenManager from the environment the
// platform's Sidecar Manager injects into every kit sidecar. It is the single
// source of truth for how a sidecar acquires and renews its gateway workload
// token, shared by the agent runtime (agent.ConnectRuntimeDeps), the gateway
// env client (gateway.NewClientFromEnv) and the MCP tools runtime
// (mcptools.NewRuntimeFromEnv).
//
//   - AGENT_BOOTSTRAP_KIND set ("k8s_sa" or "secret"): the workload token is
//     minted from the sidecar's bootstrap identity at
//     GatewayURL + "/auth/token/issue", at once and again at 50% of each
//     token's lifetime. AGENT_REGISTRATION_ID ("{tenant}.{name}") is the
//     identity the mint endpoint authorizes against. A failure is returned as
//     an error: the sidecar cannot prove its identity and must not start
//     serving.
//
//   - AGENT_BOOTSTRAP_KIND unset: returns (nil, nil). The caller runs without
//     a token and sends no Authorization header, which the Platform Gateway
//     refuses with 401: it has no tokenless mode. This is the case for a
//     sidecar run outside the platform.
func NewTokenManagerFromEnv(ctx context.Context, cfg EnvTokenManagerConfig) (*TokenManager, error) {
	provider, err := BootstrapFromEnv()
	if err != nil {
		return nil, fmt.Errorf("resolve bootstrap identity: %w", err)
	}
	if provider == nil {
		return nil, nil
	}

	regID := os.Getenv("AGENT_REGISTRATION_ID")
	if regID == "" {
		regID = cfg.AgentRegistrationID
	}
	tm, err := NewTokenManager(ctx, &TokenManagerConfig{
		Bootstrap:           provider,
		IssueURL:            strings.TrimRight(cfg.GatewayURL, "/") + "/auth/token/issue",
		AgentRegistrationID: regID,
	})
	if err != nil {
		return nil, fmt.Errorf("mint workload token: %w", err)
	}
	kitlog.From(ctx).Info("workload token: minted from bootstrap identity", kitlog.KeyRegistrationID, regID)
	return tm, nil
}
