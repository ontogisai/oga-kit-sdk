// Package kitlog standardizes structured logging for domain-kit sidecars
// (agents, MCP tool servers, data/ontology loaders, source connectors) so their
// logs match the oga-platform baseline — JSON to stdout, level from the
// environment, seeded with the common kit identity fields (tenant_id, kit_id,
// component, registration_id) — and therefore correlate with platform logs.
//
// # One-line setup
//
// Call [Init] as the first statement in a kit sidecar's main(). It reads the
// Sidecar-Manager-injected environment, builds the standardized logger, seeds
// the identity fields, and installs it as slog.Default():
//
//	func main() {
//		kitlog.Init() // that's it — slog.Info(...) now carries identity
//		slog.Info("my-sidecar starting")
//	}
//
// After Init(), plain slog.Info(...) everywhere already emits JSON on stdout
// carrying every non-empty identity field. Kit authors never hand-pass
// tenant_id/kit_id/component/registration_id on individual log calls.
//
// # Attaching errors
//
// Use [Err] (nil-safe) to attach an error under the canonical "error" key:
//
//	slog.Error("load failed", kitlog.Err(err))
//
// A handler-level canonicalizer also rewrites any error-typed attribute to the
// canonical "error" key, so slog.Error("msg", "err", err) and
// slog.Error("msg", "error", err) come out identically — authors never have to
// remember the exact key string.
//
// # Optional per-request enrichment
//
// [Into]/[From]/[WithRequest] carry a logger on a context.Context and add
// per-request trace_id/span_id/task_id. This is optional; the Init() base
// logger already produces fully-identified logs without it.
//
// The package is standard-library only and every function accepts/returns
// *slog.Logger, so it interoperates with plain log/slog code and a kit that
// ignores kitlog keeps working.
package kitlog
