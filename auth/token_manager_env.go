package auth

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// EnvTokenManagerConfig configures NewTokenManagerFromEnv.
type EnvTokenManagerConfig struct {
	// GatewayURL is the Platform Access Gateway base URL. Required — the mint
	// (/auth/token/issue) and legacy refresh (/auth/token/refresh) endpoints
	// are derived from it.
	GatewayURL string

	// AgentRegistrationID is the "{tenant}.{name}" identity the mint endpoint
	// authorizes the bootstrap credential against. Used only as a fallback when
	// the AGENT_REGISTRATION_ID environment variable (injected by the Sidecar
	// Manager) is unset.
	AgentRegistrationID string

	// TokenPath is the legacy token-file path (migration fallback). When set
	// and no bootstrap env is present, the manager loads the initial token from
	// this file and rotates it via /auth/token/refresh.
	TokenPath string
}

// NewTokenManagerFromEnv builds a TokenManager using the OGA-404 mode selection
// driven by the Sidecar-Manager-injected environment. It is the single source
// of truth for "how does a sidecar acquire + rotate its gateway workload
// token", shared by the agent runtime (agent.ConnectRuntimeDeps), the
// gateway env client (gateway.NewClientFromEnv), and the MCP tools runtime
// (mcptools.NewRuntimeFromEnv).
//
// Mode selection:
//
//   - Bootstrap-mint (preferred): when AGENT_BOOTSTRAP_KIND is set, the workload
//     token is minted from a durable bootstrap identity (a Kubernetes projected
//     ServiceAccount token, or a SecretStore secret) at /auth/token/issue — at
//     boot and on every renewal. No token file. AGENT_REGISTRATION_ID
//     ("{tenant}.{name}") is the identity the mint endpoint authorizes against.
//     A failure here is FATAL (returned as a non-nil error): the sidecar cannot
//     prove identity and must not start serving.
//
//   - Legacy file+refresh (migration fallback): when only TokenPath is set, the
//     initial token is read from the mounted file and rotated via
//     /auth/token/refresh. Init failure is NON-fatal — the helper logs a warning
//     and returns (nil, nil) so the caller keeps running on the static file
//     token until it expires.
//
//   - Neither: returns (nil, nil) — the caller runs without a managed token
//     (dev / tokenless).
func NewTokenManagerFromEnv(ctx context.Context, cfg EnvTokenManagerConfig) (*TokenManager, error) {
	base := strings.TrimRight(cfg.GatewayURL, "/")

	provider, err := BootstrapFromEnv()
	if err != nil {
		return nil, fmt.Errorf("resolve bootstrap identity: %w", err)
	}

	if provider != nil {
		regID := os.Getenv("AGENT_REGISTRATION_ID")
		if regID == "" {
			regID = cfg.AgentRegistrationID
		}
		tm, mErr := NewTokenManager(ctx, &TokenManagerConfig{
			Bootstrap:           provider,
			IssueURL:            base + "/auth/token/issue",
			AgentRegistrationID: regID,
		})
		if mErr != nil {
			return nil, fmt.Errorf("mint workload token: %w", mErr)
		}
		slog.Info("workload token: bootstrap-mint mode", "agent_registration_id", regID)
		return tm, nil
	}

	if cfg.TokenPath != "" {
		tm, mErr := NewTokenManager(ctx, &TokenManagerConfig{
			TokenPath:  cfg.TokenPath,
			RefreshURL: base + "/auth/token/refresh",
		})
		if mErr != nil {
			// Non-fatal: fall back to the static file token. The sidecar still
			// works until the token expires; we log so the gap is visible.
			slog.Warn("token manager init failed; running without token rotation",
				"error", mErr, "token_path", cfg.TokenPath)
			return nil, nil
		}
		return tm, nil
	}

	return nil, nil
}
