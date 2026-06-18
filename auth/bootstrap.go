package auth

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Bootstrap kinds — must match the platform gateway's mint endpoint contract
// (POST /auth/token/issue). See the OGA-404 design.
const (
	// BootstrapKindK8sSA presents a Kubernetes projected ServiceAccount token
	// (sent in the request body as bootstrap_token).
	BootstrapKindK8sSA = "k8s_sa"

	// BootstrapKindSecret presents a SecretStore-delivered bootstrap secret
	// (sent in the Authorization: Bearer header, never the body).
	BootstrapKindSecret = "secret"
)

// BootstrapProvider yields the durable bootstrap identity an agent sidecar
// presents to the gateway mint endpoint to obtain a workload token. It is read
// on EVERY mint (boot + each renewal) so kubelet-rotated credentials are picked
// up — the provider must never cache a stale value.
type BootstrapProvider interface {
	// Token returns the bootstrap kind (BootstrapKind*) and the current
	// credential value.
	Token(ctx context.Context) (kind, token string, err error)
}

// K8sSABootstrap reads a Kubernetes projected ServiceAccount token from a file
// on each call. kubelet keeps the file fresh (rotates + re-projects on
// reschedule); the agent only ever reads it — it is never written by the app.
type K8sSABootstrap struct {
	// Path is the projected SA token mount path (AGENT_SA_TOKEN_PATH).
	Path string
}

// Token reads and returns the current projected SA token.
func (b K8sSABootstrap) Token(_ context.Context) (string, string, error) {
	data, err := os.ReadFile(b.Path)
	if err != nil {
		return "", "", fmt.Errorf("read projected SA token %s: %w", b.Path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", "", fmt.Errorf("projected SA token %s is empty", b.Path)
	}
	return BootstrapKindK8sSA, tok, nil
}

// SecretBootstrap presents a static SecretStore-delivered bootstrap secret
// (non-K8s / air-gapped). The value is held in memory for the process lifetime.
type SecretBootstrap struct {
	Secret string
}

// Token returns the static bootstrap secret.
func (b SecretBootstrap) Token(_ context.Context) (string, string, error) {
	if b.Secret == "" {
		return "", "", fmt.Errorf("bootstrap secret is empty")
	}
	return BootstrapKindSecret, b.Secret, nil
}

// BootstrapFromEnv builds a BootstrapProvider from the agent's environment,
// selecting the kind via AGENT_BOOTSTRAP_KIND:
//
//   - "k8s_sa" → K8sSABootstrap reading AGENT_SA_TOKEN_PATH
//   - "secret" → SecretBootstrap reading AGENT_BOOTSTRAP_SECRET
//
// Returns (nil, nil) when AGENT_BOOTSTRAP_KIND is unset — callers then fall
// back to the legacy file+refresh TokenManager mode.
func BootstrapFromEnv() (BootstrapProvider, error) {
	switch os.Getenv("AGENT_BOOTSTRAP_KIND") {
	case BootstrapKindK8sSA:
		path := os.Getenv("AGENT_SA_TOKEN_PATH")
		if path == "" {
			return nil, fmt.Errorf("AGENT_BOOTSTRAP_KIND=k8s_sa but AGENT_SA_TOKEN_PATH is unset")
		}
		return K8sSABootstrap{Path: path}, nil
	case BootstrapKindSecret:
		secret := os.Getenv("AGENT_BOOTSTRAP_SECRET")
		if secret == "" {
			return nil, fmt.Errorf("AGENT_BOOTSTRAP_KIND=secret but AGENT_BOOTSTRAP_SECRET is unset")
		}
		return SecretBootstrap{Secret: secret}, nil
	case "":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown AGENT_BOOTSTRAP_KIND %q", os.Getenv("AGENT_BOOTSTRAP_KIND"))
	}
}
