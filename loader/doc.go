// Package loader defines the HTTP contract between the ONTOGIS AI Platform
// and kit-supplied data loader sidecars.
//
// # Architecture
//
// Domain kits ship loader sidecar containers in their tar.gz bundles. The
// platform's DataImportWorkflow looks up the running sidecar in the sidecar
// registry and calls into it over HTTP using the contract defined in this
// package. The platform never sees domain-specific format parsing logic —
// that lives entirely inside the kit's loader binary.
//
//	┌──────────────────────────────┐
//	│  Platform DataImportWorkflow │
//	│  (oga-platform)              │
//	└─────────────┬────────────────┘
//	              │  POST /load
//	              │  GET /jobs/{id}
//	              │  GET /formats
//	              │  GET /healthz
//	              ▼
//	┌──────────────────────────────┐
//	│  Loader Sidecar              │
//	│  (kit-supplied container)    │
//	│                              │
//	│  Implements LoaderHandler    │
//	│  via loader.ListenAndServe   │
//	└──────────────────────────────┘
//
// # Endpoints
//
//   - POST /load        — start a load job. Sync (200) or async (202).
//   - GET  /jobs/{id}   — query async job status.
//   - GET  /formats     — list of supported format identifiers.
//   - GET  /healthz     — liveness probe.
//
// # Kit-author usage
//
// Implement [LoaderHandler] with the format-specific parsing logic, then
// hand it to [ListenAndServe]:
//
//	func main() {
//	    impl := &MyBrickLoader{...}
//	    loader.ListenAndServe(context.Background(), "8400", impl)
//	}
//
// The Load method may run synchronously (return [LoadResponse] with
// StatusCompleted) or kick off an async job and return StatusRunning with a
// job ID. Async loaders also implement [JobLookup] so the platform can poll
// /jobs/{id}.
//
// # Platform usage
//
// Platform code uses [Client] to call into running loader sidecars from the
// DataImportWorkflow. The client is transport-only; activity-level retries
// and observability are handled by Temporal.
package loader
