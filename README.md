# oga-kit-sdk

Domain-agnostic SDK for ONTOGIS AI Platform kit development.

[![CI](https://github.com/ontogisai/oga-kit-sdk/actions/workflows/ci.yml/badge.svg)](https://github.com/ontogisai/oga-kit-sdk/actions/workflows/ci.yml)

## Overview

`oga-kit-sdk` is the public contract for all domain kit development on the ONTOGIS AI Platform. It contains:

- **Interfaces** for schema management, ontology registration, and data loading
- **Agent runtime chassis** (A2A-compliant HTTP server with LLM + MCP tool access)
- **Platform Gateway client** (single endpoint for all platform services)
- **Token management** (sliding renewal, atomic rotation, credential watching)
- **Testing utilities** (mock MCP server, mock schema manager, mock ontology registrar)

Zero dependencies on `oga-platform`. Kit developers depend only on this module.

## Installation

```bash
go get github.com/ontogisai/oga-kit-sdk@latest
```

## Quick Start

### 1. Define your agent

Create an agent profile YAML:

```yaml
# agents/my-agent.yaml
agent_id: my-domain-agent
name: My Domain Agent
description: Handles domain-specific queries
version: "1.0.0"
port: "8200"
category: customer_extension
domain: my-vertical
skills:
  - id: query
    name: Domain Query
    description: Answers domain-specific questions
    tags: [query, domain]
proactive_reasoning:
  system_prompt: |
    You are a domain expert agent. Use the available MCP tools
    to answer questions about the knowledge graph.
  tool_categories: [kg_entity, kg_document]
```

### 2. Build your agent binary

```go
package main

import (
    "context"
    "log/slog"
    "os"

    "github.com/ontogisai/oga-kit-sdk/agent"
)

func main() {
    ctx := context.Background()

    profile, err := agent.LoadDomainAgentProfile(envOr("AGENT_PROFILE_PATH", "/config/profile.yaml"))
    if err != nil {
        slog.Error("load profile failed", "error", err)
        os.Exit(1)
    }

    deps, err := agent.ConnectRuntimeDeps(ctx, &agent.RuntimeDepsConfig{
        GatewayURL:       envOr("PLATFORM_GATEWAY_URL", "http://localhost:8050"),
        EventStreamURL:   envOr("EVENT_STREAM_URL", "nats://localhost:4222"),
        EventStreamCreds: envOr("EVENT_STREAM_CREDENTIALS_PATH", "/run/oga/agents/creds"),
        TokenPath:        envOr("AGENT_SERVICE_TOKEN_PATH", "/run/oga/agents/token"),
        AgentID:          profile.AgentID,
        TenantID:         envOr("OGA_TENANT_ID", ""),
    })
    if err != nil {
        slog.Error("connect failed", "error", err)
        os.Exit(1)
    }
    defer deps.Close()

    runtime := agent.NewDefaultRuntime(profile, deps)
    agent.ListenAndServe(ctx, profile.Port, runtime)
}

func envOr(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}
```

### 3. Test with mocks

```go
package myagent_test

import (
    "encoding/json"
    "testing"

    "github.com/ontogisai/oga-kit-sdk/testing/mcpmock"
)

func TestAgentToolCall(t *testing.T) {
    server := mcpmock.NewServer(t)
    server.RegisterTool("kg_search", func(params json.RawMessage) (any, error) {
        return map[string]any{"results": []any{}}, nil
    })

    // Use server.URL() as the gateway URL in your agent config
    _ = server.URL()

    // Verify tool was called
    if server.CallCount("kg_search") != 0 {
        t.Error("expected 0 calls before invocation")
    }
}
```

### 4. Build a loader sidecar

Loader sidecars run inside a kit's tar.gz bundle. The platform's
`DataImportWorkflow` calls them over HTTP using the contract in `loader/`.

```go
package main

import (
    "context"

    "github.com/ontogisai/oga-kit-sdk/loader"
)

type myLoader struct{}

func (l *myLoader) Load(ctx context.Context, req *loader.LoadRequest) (*loader.LoadResponse, error) {
    // Parse req.SourceURI, produce vertices/edges, return stats.
    return &loader.LoadResponse{
        Status: loader.StatusCompleted,
        Stats:  &loader.LoadStats{VerticesCreated: 1234, EdgesCreated: 5678},
    }, nil
}

func (l *myLoader) Job(ctx context.Context, jobID string) (*loader.LoadResponse, error) {
    return nil, &loader.ErrJobNotFound{JobID: jobID}
}

func (l *myLoader) Formats(ctx context.Context) ([]string, error) {
    return []string{"my-format-v1"}, nil
}

func (l *myLoader) Health(ctx context.Context) (*loader.HealthResponse, error) {
    return &loader.HealthResponse{Status: "ok"}, nil
}

func main() {
    _ = loader.ListenAndServe(context.Background(), &loader.ServerConfig{Port: "8400"}, &myLoader{})
}
```

## Package Structure

| Package | Purpose |
|---------|---------|
| `schema/` | `SchemaManager` interface, `VertexTypeDef`, `EdgeTypeDef`, `PropertyType`, `IndexDef` |
| `ontology/` | `OntologyRegistrar` interface, `EntityTypeDef`, `TypeHierarchyEntry` |
| `ingest/` | `DataLoader` in-process interface, `Vertex`, `Edge`, `LoadResult` |
| `loader/` | HTTP loader-sidecar contract: `LoaderHandler`, `Client`, `ListenAndServe`, types for `/load`, `/jobs/{id}`, `/formats`, `/healthz` |
| `manifest/` | `KitManifest` types, `Parse`, `Validate` |
| `agent/` | `AgentRuntime` interface, `DefaultRuntime`, `ListenAndServe` |
| `gateway/` | `PlatformGatewayClient` (MCP, LLM, workflows, inter-agent, registry) |
| `auth/` | `TokenManager`, `CredentialWatcher` |
| `testing/mcpmock` | Mock MCP server |
| `testing/schemamock` | Mock `SchemaManager` |
| `testing/ontologymock` | Mock `OntologyRegistrar` |
| `testing/loadermock` | Programmable mock loader sidecar |

## Design Principles

1. **Zero platform dependencies** — this module never imports `oga-platform`
2. **Domain-agnostic** — no Brick, FHIR, or military-specific types
3. **Interface-first** — consumers depend on interfaces, platform provides implementations
4. **Testable** — mock implementations for all interfaces
5. **Stable API** — semver, backward-compatible within major versions

## License

Apache License 2.0 — see [LICENSE](LICENSE).
