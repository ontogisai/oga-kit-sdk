package gateway

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/auth"
)

// Environment variables read by NewClientFromEnv. These are injected by the
// platform Sidecar Manager (internal/sidecar/manager.go buildEnv) into every
// sidecar container — agents, MCP tool servers, and loaders alike.
const (
	envGatewayURL       = "PLATFORM_GATEWAY_URL"
	envTenantID         = "OGA_TENANT_ID"
	envServiceTokenPath = "AGENT_SERVICE_TOKEN_PATH" // legacy file+refresh fallback
	defaultGatewayURL   = "http://localhost:8050"
)

// noopCloser is returned when there is no token manager to stop (dev /
// tokenless), so callers can always defer Close without a nil check.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// tokenManagerCloser adapts a *auth.TokenManager to io.Closer so callers stop
// its renewal goroutine on shutdown via the returned Closer.
type tokenManagerCloser struct{ tm *auth.TokenManager }

func (c tokenManagerCloser) Close() error {
	c.tm.Stop()
	return nil
}

// NewClientFromEnv builds an authenticated PlatformGatewayClient from the
// Sidecar-Manager-injected environment, for non-agent sidecars (MCP tool
// servers, loaders) and any downstream code that wants the agent's auth wiring
// without the full agent runtime.
//
// It reads PLATFORM_GATEWAY_URL (default http://localhost:8050), OGA_TENANT_ID,
// AGENT_REGISTRATION_ID, the OGA-404 bootstrap vars (AGENT_BOOTSTRAP_KIND, etc.),
// and the legacy AGENT_SERVICE_TOKEN_PATH; mints + rotates the workload token via
// auth.NewTokenManagerFromEnv; and wires it onto the client with SetTokenProvider
// so every request carries a fresh bearer.
//
// The returned io.Closer stops the token manager's renewal goroutine — call it
// on shutdown. It is never nil (a no-op Closer is returned in dev/tokenless
// mode), so callers can unconditionally defer Close.
//
// A bootstrap-mint failure is fatal and returned as an error: the sidecar
// cannot prove identity and must not start serving. A legacy-mode init failure
// is non-fatal (the client still serves on the static file token, or tokenless
// in dev).
func NewClientFromEnv(ctx context.Context) (*PlatformGatewayClient, io.Closer, error) {
	gatewayURL := os.Getenv(envGatewayURL)
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}
	tenantID := os.Getenv(envTenantID)
	tokenPath := os.Getenv(envServiceTokenPath)

	client := NewPlatformGatewayClient(gatewayURL, tokenPath, tenantID)

	tm, err := auth.NewTokenManagerFromEnv(ctx, auth.EnvTokenManagerConfig{
		GatewayURL: strings.TrimRight(gatewayURL, "/"),
		// AGENT_REGISTRATION_ID (manager-injected) is read inside the helper;
		// no profile-derived fallback is available for a non-agent sidecar.
		TokenPath: tokenPath,
	})
	if err != nil {
		return nil, nil, err
	}
	if tm == nil {
		return client, noopCloser{}, nil
	}

	client.SetTokenProvider(tm.Token)
	return client, tokenManagerCloser{tm: tm}, nil
}
