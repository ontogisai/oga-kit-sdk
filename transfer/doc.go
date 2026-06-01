// Package transfer is the kit-author surface for streaming a load
// artifact to the platform. Loader sidecars use a [Writer] to emit
// vertices, edges, and entity-type definitions while the platform
// handles persistence (DDL + EntityTypeDef + activation for ontology
// loads, batched UPSERT for data loads).
//
// # Wire shape
//
// The platform exposes three MCP tools that this package wraps:
//
//   - loader.prepare_upload — issues a presigned PUT URL bound to a
//     tenant-scoped object key. The kit author never sees the URL or
//     the object key directly; [Writer] requests one as needed.
//
//   - loader.complete — accepts either a pointer to an uploaded object
//     (post-presigned-upload) or an inline body for small payloads
//     (≤ [InlineBodyLimit] bytes). Returns a platform-issued job_id
//     immediately. Both ontology and data flavors are async.
//
//   - loader.status — polled by the platform's install / import
//     workflow until the job reaches a terminal state. Loader sidecars
//     do not poll — the writer's [Writer.Close] returns the moment the
//     bytes are committed, the platform-side processing happens out of
//     band.
//
// # Single-pass vs multi-pass
//
// Most kits stream every entry in a single pass: parse the source,
// call WriteVertex / WriteEdge / WriteEntityType, then Close. The
// writer buffers in memory up to [InlineBodyLimit] (700 KiB) and switches
// to a presigned-upload streaming path when the buffer fills.
//
// Kits whose source format requires more than one pass over the input
// (for example, building a vertex source-id → platform-id map in pass
// 1, then emitting edges with resolved IDs in pass 2) implement
// [github.com/ontogisai/oga-kit-sdk/loader.StreamingLoaderHandler]
// instead of [github.com/ontogisai/oga-kit-sdk/loader.LoaderHandler].
// The SDK auto-detects the streaming variant via type assertion.
//
// # Memory ceiling guidance
//
// The writer is streaming above [InlineBodyLimit]; loader memory is
// bounded by what the kit's parser holds, not by what it has emitted.
// For source files larger than [MultiPassThreshold] (5 MiB) kit
// authors should switch to [json.Decoder]-style streaming and a
// [loader.StreamingLoaderHandler] so the parser stays bounded too.
//
// # Tenant boundary
//
// The writer never accepts a tenant_id from the kit. Tenant flows
// through the gateway auth context (X-Tenant-ID header set by the
// platform when starting the loader sidecar). Body claims are
// stripped server-side before dispatch.
package transfer
