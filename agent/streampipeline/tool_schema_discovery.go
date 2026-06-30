package streampipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ontogisai/oga-kit-sdk/agent"
	"github.com/ontogisai/oga-kit-sdk/gateway"
)

// toolSchemaLister is the optional capability a PlatformAccess implementation
// advertises when it can discover MCP tool input schemas (OGA-431). The real
// *gateway.PlatformGatewayClient implements it via the gateway's tool-discovery
// endpoint; adapters that cannot discover schemas (e.g. the platform Knowledge
// Agent's direct MCP adapter, which already supplies schemas by another path,
// and test fakes) simply do not implement it. Persona builders type-assert
// deps.Gateway to this interface and degrade gracefully to names-only when the
// assertion fails — so this is a non-breaking addition to PlatformAccess.
type toolSchemaLister interface {
	ListTools(ctx context.Context) ([]gateway.ToolSchema, error)
}

// toolSchemaCacheTTL bounds how long a discovered schema set is reused before a
// refresh. The platform tool palette is process-stable, so a short TTL keeps
// the cost to roughly one discovery call per agent process while still picking
// up a kit reinstall within the window.
const toolSchemaCacheTTL = 5 * time.Minute

// toolSchemaCache memoizes the agent's discovered MCP tool schemas for the
// lifetime of a handler (the reactive stream handler and the proactive message
// handler each hold one — both are constructed once per process and reused
// across requests). Concurrency-safe; refresh is single-flighted under the
// mutex.
type toolSchemaCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	fetchedAt time.Time
	byName    map[string]agent.ToolSchema
}

// newToolSchemaCache constructs an empty cache with the default TTL.
func newToolSchemaCache() *toolSchemaCache {
	return &toolSchemaCache{ttl: toolSchemaCacheTTL}
}

// schemasFor returns agent.ToolSchema entries for the requested tool names, in
// the given palette order, discovered via the gateway and memoized. Tools the
// gateway did not return a schema for are omitted (they still render as bare
// names in the palette). When the gateway does not support discovery, or
// discovery fails, it returns nil — the planner then falls back to the
// pre-OGA-431 names-only behavior rather than blocking the request.
func (c *toolSchemaCache) schemasFor(ctx context.Context, gw PlatformAccess, names []string) []agent.ToolSchema {
	if gw == nil || len(names) == 0 {
		return nil
	}
	lister, ok := gw.(toolSchemaLister)
	if !ok {
		return nil
	}

	byName := c.load(ctx, lister)
	if len(byName) == 0 {
		return nil
	}

	out := make([]agent.ToolSchema, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		if s, found := byName[n]; found {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// load returns the cached name→schema map, refreshing it via the lister when
// the cache is empty or expired. A failed refresh keeps any previously cached
// value (better a slightly stale palette than none) and returns it.
func (c *toolSchemaCache) load(ctx context.Context, lister toolSchemaLister) map[string]agent.ToolSchema {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.byName != nil && time.Since(c.fetchedAt) < c.ttl {
		return c.byName
	}

	tools, err := lister.ListTools(ctx)
	if err != nil {
		slog.WarnContext(ctx, "tool schema discovery failed; planner falls back to names-only",
			"error", err,
			"have_cached", c.byName != nil,
		)
		return c.byName // possibly nil; possibly stale-but-usable
	}

	byName := make(map[string]agent.ToolSchema, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		byName[t.Name] = agent.ToolSchema{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Mutates:     t.Mutates, // OGA-446 confirm-before-write signal
		}
	}
	c.byName = byName
	c.fetchedAt = time.Now()
	return c.byName
}

// mergeToolSchemas appends extra schemas (e.g. agent-delegation descriptors)
// to a discovered set, with discovered entries taking precedence on name
// collision. Either argument may be nil.
func mergeToolSchemas(discovered, extra []agent.ToolSchema) []agent.ToolSchema {
	if len(extra) == 0 {
		return discovered
	}
	seen := make(map[string]struct{}, len(discovered))
	out := make([]agent.ToolSchema, 0, len(discovered)+len(extra))
	for _, s := range discovered {
		seen[s.Name] = struct{}{}
		out = append(out, s)
	}
	for _, s := range extra {
		if _, dup := seen[s.Name]; dup {
			continue
		}
		out = append(out, s)
	}
	return out
}
