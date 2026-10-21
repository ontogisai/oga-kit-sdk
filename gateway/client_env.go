package gateway

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/auth"
)

// Environment variables read by NewClientFromEnv. These are injected by the
// platform Sidecar Manager into every sidecar container: agents, MCP tool
// servers and loaders alike.
const (
	envGatewayURL     = "PLATFORM_GATEWAY_URL"
	envTenantID       = "OGA_TENANT_ID"
	defaultGatewayURL = "http://localhost:8050"
)

// noopCloser is returned when there is no token manager to stop (no bootstrap
// identity), so callers can always defer Close without a nil check.
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
// AGENT_REGISTRATION_ID and the bootstrap identity (AGENT_BOOTSTRAP_KIND and
// its credential variable); mints and renews the workload token via
// auth.NewTokenManagerFromEnv; and wires it onto the client with
// SetTokenProvider so every request carries the current token.
//
// The returned io.Closer stops the token manager's renewal goroutine; call it
// on shutdown. It is never nil (a no-op Closer is returned when there is no
// token manager), so callers can unconditionally defer Close.
//
// A failed first mint is returned as an error: the sidecar cannot prove its
// identity and must not start serving. With no bootstrap identity the client
// sends no Authorization header, and the Platform Gateway refuses its requests
// with 401: the gateway has no tokenless mode.
func NewClientFromEnv(ctx context.Context) (*PlatformGatewayClient, io.Closer, error) {
	gatewayURL := os.Getenv(envGatewayURL)
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}
	client := NewPlatformGatewayClient(gatewayURL, os.Getenv(envTenantID))

	tm, err := auth.NewTokenManagerFromEnv(ctx, auth.EnvTokenManagerConfig{
		GatewayURL: strings.TrimRight(gatewayURL, "/"),
		// AGENT_REGISTRATION_ID (manager-injected) is read inside the helper;
		// no profile-derived fallback is available for a non-agent sidecar.
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
