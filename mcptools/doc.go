// Package mcptools provides a default runtime for domain-kit Tier 3 MCP tool
// server sidecars — the MCP-tools analogue of the agent package's
// DefaultRuntime.
//
// A kit author registers deterministic tool handlers and calls ListenAndServe;
// the runtime provides everything else:
//
//   - An authenticated Platform Access Gateway client (workload token minted +
//     rotated from the OGA-404 bootstrap env via gateway.NewClientFromEnv), so
//     a handler's internal kg_* calls authenticate without the kit wiring auth.
//   - The MCP JSON-RPC 2.0 transport at POST /mcp (tools/list, tools/call,
//     ping), with the MCP content result envelope and isError on handler error.
//   - Liveness/readiness probes at GET /healthz and GET /readyz.
//   - Tenant/principal/request-id propagation from the gateway-forwarded
//     headers into each handler's context.
//   - Graceful shutdown on SIGTERM/SIGINT that also stops the token manager.
//
// # Kit-author usage
//
//	func main() {
//		ctx := context.Background()
//		rt, err := mcptools.NewRuntimeFromEnv(ctx)
//		if err != nil {
//			slog.Error("runtime init", "error", err)
//			os.Exit(1)
//		}
//		h := NewHandlers(rt.Gateway()) // handlers call rt.Gateway().CallTool(...)
//		_ = rt.RegisterFunc("fm_get_building_overview", "…", schema, h.GetBuildingOverview)
//		// … register the rest …
//		rt.ListenAndServe(ctx, "8300")
//	}
//
// # Platform contract
//
// The platform MCP Tool Server's catalog proxies an agent's tools/call to this
// sidecar's POST /mcp endpoint with X-Tenant-ID set from the caller's JWT (see
// internal/mcptoolserver CatalogAPI). The runtime mirrors that contract so kit
// tools are reachable end-to-end once registered.
package mcptools
