// Package egress is the kit-author SDK for building egress-sync components —
// long-running, per-tenant sidecars that push knowledge-graph state OUT to an
// external system of record and report that system's identifier per entity.
//
// An egress component is the outbound counterpart of a Source Connector. Where a
// connector adapts an external system's records INTO the graph, a component maps
// graph entities OUT to the external system's shape and speaks its API. In both
// directions the platform owns the plumbing and the kit owns the domain: the
// platform reads the graph, orders and batches the work, retries transient
// failures, and persists the correlation; the component maps and pushes.
//
// # Implementing a component
//
// A kit author implements [Component] and hands it to [ListenAndServe]:
//
//	c := &myComponent{...}
//	cfg := &egress.Config{Port: "8600"}
//	egress.ListenAndServe(ctx, cfg, c)
//
// The server serves POST /egress/sync and GET /healthz, decodes each push, and
// builds a response the platform will accept. A minimal Sync:
//
//	func (c *myComponent) Sync(ctx context.Context, req *egress.SyncRequest, b *egress.Batch) error {
//	    for _, e := range req.Entities {
//	        if e.Correlation != nil {
//	            id, err := c.api.Update(ctx, e.Correlation.ExternalRecordID, mapOut(e))
//	            if err != nil {
//	                b.FailedErr(e.ID, err) // this entity only
//	                continue
//	            }
//	            b.Updated(e.ID, id)
//	            continue
//	        }
//	        id, err := c.api.Create(ctx, mapOut(e))
//	        if err != nil {
//	            b.FailedErr(e.ID, err)
//	            continue
//	        }
//	        b.Created(e.ID, id)
//	    }
//	    return nil
//	}
//
// # The platform persists the correlation, not the component
//
// A component RETURNS an external_record_id and the platform writes it onto the
// entity through its own correlation primitive. A component therefore needs no
// knowledge-graph write privilege on the common path — it never calls back into
// the platform to record what it just did.
//
// The write happens AFTER the push, because the external id does not exist until
// the component returns it. That makes the failure window explicit: a crash
// between the push and the persist leaves an external record created but
// uncorrelated. The next run pushes that entity again with no correlation
// attached, so a component MUST be able to recognize an already-created record
// and report [OutcomeUpdated] with its existing id — otherwise every interrupted
// run leaves duplicates behind. This is also why a component should treat
// [SyncRequest.BatchID], which is stable across retries, as a deduplication key.
//
// # Errors: per entity or per batch
//
// The distinction is the one thing worth getting right.
//
//   - A per-entity problem goes on the [Batch] (b.Failed / b.FailedErr) and Sync
//     returns nil. The batch is accepted and the entities that succeeded keep
//     their correlations.
//   - A batch-wide fault (the external system is down, credentials expired)
//     returns an error from Sync. The platform retries the whole batch with the
//     same batch id. Wrap it with [Throttled] when the external system asked for
//     backoff, and the platform will honor the delay.
//
// # Entity types are source-native class IDs
//
// Every entity_type on this contract — the batch's, each entity's, and the
// manifest's entities_sync[] — is the SOURCE-NATIVE CLASS ID, verbatim. It may contain a colon (`brick:AHU`,
// `rec:Zone`) or be colon-free (`Equipment`, `WorkOrder`), and both forms are
// class IDs naming different catalog entries; a colon-free name is not a plainer
// spelling of a namespaced one.
//
// Match it exactly and route on the whole string. Do not sanitize it, fold its
// case, split on the colon, or translate it to an internal name — the platform
// does no translation outbound, so what arrives is already the identifier the
// customer's ontology uses, and it is never the platform's internal storage
// identifier.
//
// # Tenancy
//
// A component never asserts its own tenant. [SyncRequest.TenantID] is
// informational; authority is the component's workload identity, and the
// platform never reads a tenant back out of a response.
//
// # Declaring the component
//
// The sidecar is declared in the kit manifest under spec.egress_syncs — see
// manifest.EgressSyncSpec for the fields, notably the push ORDER of
// entities_sync, parent_edges (which owner references the platform resolves and
// sends as [Entity.ParentRefs]), and hierarchical for a type whose owner edge is
// self-referencing.
//
// A component may also declare an ontology_sync lane, which pushes the tenant's
// ontology TYPES — the external system's type catalog — in full before any
// instance in the same run. It needs NO second handler: the catalog arrives as
// ordinary batches over this same [Syncer] contract, with owner references under
// the same [Entity.ParentRefs] shape, so a component implements one path. What the
// lane replaces is the kit-side habit of seeding a catalog lazily from the entity
// payload and relying on the external system to reject a duplicate name; the
// platform correlates each type record instead, which does not depend on the
// external system having a unique constraint. See manifest.EgressOntologySyncSpec.
// The platform side of this lane is not built yet (OGA-846).
//
// Each parent_edges entry carries a traversal DIRECTION, defaulting to outbound.
// The key under which an owner arrives in [Entity.ParentRefs] is the declared EDGE
// name, not the semantic relation — so an owner reached inbound over hasPoint
// arrives under "hasPoint", even though the relation reads naturally as
// "isPointOf". See manifest.ParentEdgeSpec.
package egress
