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
// writer keeps the body in memory up to [InlineBodyLimit] (700 KiB) and
// commits it inline; past that it spools the body to an unlinked temp file
// in [os.TempDir] and uploads it from there through the presigned path.
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
// Above [InlineBodyLimit] the writer holds a 256 KiB write buffer and one
// encoded record, whatever the artifact's size, so loader memory is bounded
// by what the kit's parser holds, not by what it has emitted. The emitted
// bytes go to disk instead: a sidecar needs a writable TMPDIR with room for
// the artifact (the platform mounts one at /tmp), and a writer that cannot
// create its spool fails the write rather than falling back to memory.
//
// A writer that is abandoned instead of closed releases its spool through
// [Discarder.Discard]; the SDK's connector and loader servers call it on
// every path that drops a writer. Because the spool is unlinked as soon as
// it is created, a writer dropped without Discard — or a process that is
// killed — leaves no file behind either: the space returns when the
// descriptor is closed or the process exits.
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
