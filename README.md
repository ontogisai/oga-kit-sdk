# oga-kit-sdk

Domain-agnostic SDK for ONTOGIS AI Platform kit development.

[![CI](https://github.com/ontogisai/oga-kit-sdk/actions/workflows/ci.yml/badge.svg)](https://github.com/ontogisai/oga-kit-sdk/actions/workflows/ci.yml)

## Overview

`oga-kit-sdk` is the public contract for all domain kit development on the ONTOGIS AI Platform. It contains:

- **Loader contract** for ontology + data loaders (HTTP sidecars on `kind: ontology|data`)
- **Streaming transfer pipeline** (`transfer.Writer`) for shipping load artifacts to the platform via presigned-URL handoff
- **Ontology registrar** convenience layer for kit authors who think in "register a batch of types" terms
- **Agent runtime chassis** (A2A-compliant HTTP server with LLM + MCP tool access)
- **Platform Gateway client** (single endpoint for all platform services)
- **Token management** (sliding renewal, atomic rotation, credential watching)
- **Testing utilities** (mock MCP server, mock loader sidecar, fake commit client)

Zero dependencies on `oga-platform`. Kit developers depend only on this module.

## Installation

```bash
go get github.com/ontogisai/oga-kit-sdk@latest
```

## Loader sidecars

Loader sidecars run as containers inside a kit's tar.gz bundle. The platform calls them over HTTP using the contract in `loader/`. The platform recognises **two kinds** of loaders, distinguished by the `kind:` field on the loader spec in your kit's `manifest.yaml`:

| Kind | When invoked | Produces |
|------|-------------|----------|
| `ontology` | At kit install time, by the `domain-kit-installer` Temporal worker | Entity / relationship type definitions persisted via the platform's ontology activator |
| `data` | At `oga-admin import` time, by `DataImportWorkflow` | Vertices and edges persisted via the platform's batched ingest |

The HTTP contract is identical for both kinds — the same `LoaderHandler` interface, the same `POST /load` and `GET /jobs/{id}` endpoints. The kind only changes **when** the platform calls the loader and **what kind of work** the kit author is expected to do inside the handler.

When the field is missing from a kit manifest, the platform defaults to `data`. Always set it explicitly on new kits.

## How records reach the platform

Loaders never write to ArcadeDB directly. Records flow through a streaming `transfer.Writer` that handles upload + commit:

```
Loader sidecar                         Platform gateway              Storage (MinIO/S3)
─────────────────                      ────────────────              ──────────────────
WriteVertex/WriteEntityType/...   →    (buffered in writer)
                                       (writer chooses transport)
                  ┌────────────────────────────────────────────────────────────┐
                  │ Inline path (artifact ≤ 1 MiB):                            │
Close()           │   loader.complete  with inline_body  ────────────►         │
                  │   (single MCP call)                                        │
                  ├────────────────────────────────────────────────────────────┤
                  │ Presigned path (artifact > 1 MiB):                         │
Close()           │   loader.prepare_upload  ────►  (issues presigned URL)     │
                  │   PUT bytes  ──────────────────────────────────────►       │
                  │   loader.complete  with upload_token  ────────►            │
                  └────────────────────────────────────────────────────────────┘

Returns:  receipt (job_id, content_hash, mode)
```

The platform owns the job ledger; both ontology and data loads are uniformly async. The kit's caller (the install workflow for ontology, the import workflow for data) polls `loader.status` until terminal.

## Single-pass loader (most kits)

For source files under 5 MiB or formats that don't need cross-pass state, implement `loader.LoaderHandler` and stream every record in one pass:

```go
package main

import (
    "context"
    "encoding/json"
    "os"

    "github.com/ontogisai/oga-kit-sdk/loader"
    "github.com/ontogisai/oga-kit-sdk/transfer"
)

type myDataLoader struct{}

func (l *myDataLoader) Load(ctx context.Context, lc *loader.LoadContext) (*loader.LoadResponse, error) {
    w := lc.Transfer
    raw, _ := os.ReadFile(strings.TrimPrefix(lc.Request.SourceURI, "file://"))

    var file struct {
        Entities      []struct{ ID, Type, Label string; Properties map[string]any } `json:"entities"`
        Relationships []struct{ Source, Target, Type string }                       `json:"relationships"`
    }
    json.Unmarshal(raw, &file)

    for _, e := range file.Entities {
        if err := w.WriteVertex(ctx, transfer.Vertex{
            ID: e.ID, EntityType: e.Type, Label: e.Label, Properties: e.Properties,
        }); err != nil {
            return nil, err
        }
    }
    for _, r := range file.Relationships {
        if err := w.WriteEdge(ctx, transfer.Edge{
            SourceID: r.Source, TargetID: r.Target, RelationshipType: r.Type,
        }); err != nil {
            return nil, err
        }
    }

    return &loader.LoadResponse{Status: loader.StatusRunning}, nil
    // SDK closes the writer; receipt fields land in the response
    // automatically. The platform-issued job_id is what callers poll.
}

// (Job, Formats, Health implementations omitted for brevity.)

func main() {
    cc, _ := transfer.NewHTTPCommitClient(
        os.Getenv("PLATFORM_GATEWAY_URL"),
        os.Getenv("OGA_TENANT_ID"),
        "my-kit",
    )
    factory := func(_ context.Context, kind transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
        return transfer.NewWriter(cc, kind, "my-kit"), nil
    }
    cfg := &loader.ServerConfig{
        Port: "8400",
        HandlerOptions: []loader.HandlerOption{
            loader.WithWriterFactory(factory),
            loader.WithLoaderKind(transfer.KindData),
        },
    }
    loader.ListenAndServe(context.Background(), cfg, &myDataLoader{})
}
```

The kit handler:

- Receives a `*loader.LoadContext` carrying the request and a fresh `transfer.Writer`.
- Streams records via `WriteVertex` / `WriteEdge` / `WriteEntityType` / `WriteHierarchy`.
- Returns its `LoadResponse` — the SDK closes the writer for you and merges the platform-issued `job_id` and stats into the response before sending it on the wire.

## Multi-pass loader (large files, > 5 MiB)

When source data is too large to hold a parsed in-memory representation, implement `loader.StreamingLoaderHandler` to walk the input multiple times. Typical pattern: pass 1 emits vertices and builds a source-id → platform-id map; pass 2 emits edges with resolved IDs:

```go
type myStreamingLoader struct {
    idMap map[string]string
}

// Load is required by the interface but never called when Plan / Pass exist.
func (l *myStreamingLoader) Load(_ context.Context, _ *loader.LoadContext) (*loader.LoadResponse, error) {
    return nil, errors.New("use Plan/Pass instead")
}

func (l *myStreamingLoader) Plan(_ context.Context, _ *loader.LoadContext) (*loader.LoadPlan, error) {
    return &loader.LoadPlan{
        Passes: []loader.PassSpec{
            {Name: "vertices", EntryKinds: []string{"vertex"}, Description: "Stream entities"},
            {Name: "edges",    EntryKinds: []string{"edge"},   Description: "Stream relationships"},
        },
    }, nil
}

func (l *myStreamingLoader) Pass(ctx context.Context, lc *loader.LoadContext, p *loader.PassSpec) error {
    f, err := os.Open(strings.TrimPrefix(lc.Request.SourceURI, "file://"))
    if err != nil { return err }
    defer f.Close()

    dec := json.NewDecoder(f)
    switch p.Name {
    case "vertices":
        l.idMap = make(map[string]string, 50_000)
        for {
            var v sourceEntity
            if err := dec.Decode(&v); errors.Is(err, io.EOF) { break }
            l.idMap[v.SourceID] = v.PlatformID()
            if err := lc.Transfer.WriteVertex(ctx, v.toVertex()); err != nil { return err }
        }
    case "edges":
        for {
            var r sourceRelationship
            if err := dec.Decode(&r); errors.Is(err, io.EOF) { break }
            edge := r.toEdge(l.idMap)
            if err := lc.Transfer.WriteEdge(ctx, edge); err != nil { return err }
        }
    }
    return nil
}
```

The SDK detects the streaming variant via type assertion and drives the Plan / Pass loop instead of calling Load. The same `lc.Transfer` is shared across all passes.

## Ontology loader convenience

For ontology loaders the `ontology` package wraps the writer with a "register a batch of types" surface:

```go
import "github.com/ontogisai/oga-kit-sdk/ontology"

func (l *myOntologyLoader) Load(ctx context.Context, lc *loader.LoadContext) (*loader.LoadResponse, error) {
    types := buildEntityTypeDefs(...)
    hierarchy := buildHierarchy(...)

    reg := ontology.NewRegistrar(lc.Transfer)
    if err := reg.RegisterTypes(ctx, ontology.RegisterTypesRequest{
        EntityTypes:   types,
        TypeHierarchy: hierarchy,
    }); err != nil {
        return failed(err)
    }
    return &loader.LoadResponse{Status: loader.StatusRunning}, nil
}
```

The registrar leaves the underlying writer open — the SDK closes it once at request end, and the platform-side dispatcher does atomic DDL + EntityTypeDef + activation.

## Tenant boundary

Loaders never trust `tenant_id` from a request body. The platform's gateway sets the `X-Tenant-ID` header authoritatively when starting the loader sidecar; the SDK's HTTP server reads it from the header on every `POST /load`, validates any body claim matches, and overwrites `LoadRequest.TenantID` with the header value before invoking the kit handler.

## Memory ceiling guidance

The writer is streaming above `transfer.InlineBodyLimit` (1 MiB); loader memory is bounded by what the kit's parser holds, not by what it has emitted.

For source files larger than `transfer.MultiPassThreshold` (5 MiB), kit authors should:

1. Switch from `os.ReadFile` + `json.Unmarshal` to `json.Decoder` streaming
2. Implement `StreamingLoaderHandler` so the parser's working set stays bounded across passes

The SDK has no opinion about how the kit parses — only about how records leave the loader. The 5 MiB constant is a guideline, not an enforced threshold.

## Build an agent

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
```

## Test with mocks

`transfer.FakeCommitClient` records every prepare / put / complete call so kit tests can assert on the artifact body, transport mode, and request shape without standing up a gateway:

```go
fc := &transfer.FakeCommitClient{}
w := transfer.NewOntologyWriter(fc, "test-kit")

reg := ontology.NewRegistrar(w)
reg.RegisterTypes(ctx, ontology.RegisterTypesRequest{...})
receipt, _ := w.Close(ctx)

if receipt.Mode != transfer.TransportInline {
    t.Errorf("small payload should not trigger presigned upload")
}
if fc.CompleteCalls() != 1 {
    t.Errorf("expected exactly one loader.complete call")
}
```

`transfer.NopWriter` is a test-only writer that discards records — useful when testing the request-routing path without exercising the persistence path:

```go
factory := func(_ context.Context, _ transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
    return transfer.NewNopWriter("test-job"), nil
}
```

## Locale keys (BCP-47, OGA-51)

Every locale-keyed map a kit emits — `KitMetadata.DisplayName`,
`KitMetadata.Description`, `EntityTypeDef.DisplayName`,
`EntityTypeDef.Description`, `TypeProperty.Description` — MUST use full
BCP-47 tags (`en-US`, `vi-VN`, `zh-CN`). Short-form language-only tags
(`en`, `vi`, `zh`) are rejected at parse / validation time so the
platform's locale matcher cannot silently disagree on the kit's
intended locale.

Manifest YAML:

```yaml
metadata:
  display_name:
    en-US: "My Kit"
    vi-VN: "Bộ Khởi Động"
  description:
    en-US: "..."
    vi-VN: "..."
```

Programmatic validation for kit-side type definitions:

```go
import "github.com/ontogisai/oga-kit-sdk/transfer"

if err := transfer.ValidateLocaleKeys(
    "entity_type.display_name",
    typeDef.DisplayName,
); err != nil {
    return err // names the offending field + key
}
```

The `manifest.Validate` function automatically calls the same helper
on `KitMetadata.DisplayName` and `KitMetadata.Description` — kits that
ship a malformed manifest get a clear error during install.

## Package Structure

| Package | Purpose |
|---------|---------|
| `transfer/` | Streaming `Writer`, presigned-URL handoff, fake clients for tests |
| `ontology/` | `Registrar` convenience over `transfer.Writer` for batch type registration |
| `loader/` | HTTP loader-sidecar contract: `LoaderHandler`, `StreamingLoaderHandler`, `Client`, `ListenAndServe` |
| `manifest/` | `KitManifest` types, `Parse`, `Validate` |
| `agent/` | `AgentRuntime` interface, `DefaultRuntime`, `ListenAndServe` |
| `gateway/` | `PlatformGatewayClient` (MCP, LLM, workflows, inter-agent, registry) |
| `auth/` | `TokenManager`, `CredentialWatcher` |
| `testing/mcpmock` | Mock MCP server |
| `testing/loadermock` | Programmable mock loader sidecar |

## Design Principles

1. **Zero platform dependencies** — this module never imports `oga-platform`
2. **Domain-agnostic** — no Brick, FHIR, or military-specific types
3. **Interface-first** — consumers depend on interfaces, platform provides implementations
4. **Testable** — fake clients and mock writers for all wire surfaces
5. **Tenant boundary by construction** — `X-Tenant-ID` flows through gateway auth, never from body claims
6. **Stable API** — semver, backward-compatible within major versions

## License

Apache License 2.0 — see [LICENSE](LICENSE).
