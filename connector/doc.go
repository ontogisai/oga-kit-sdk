// Package connector is the kit-author SDK for building Source Connectors —
// long-running, per-tenant sidecars that connect a customer's live systems
// (CMMS/Work Order, BMS, IoT, historians, ERPs) to the platform and submit
// records continuously.
//
// A Source Connector is the continuous-cadence sibling of a loader: where a
// loader runs once at import time, a connector runs for the tenant's lifetime,
// pulling changes on a poll cadence and/or receiving webhook pushes. Both emit
// the SAME ontology-shaped records through the transfer contract — entities and
// edges via a [transfer.Writer]; the platform owns persistence, resolution, and
// validation. Domain mapping lives in the connector (kit code), never the
// platform.
//
// # Implementing a connector
//
// A kit author implements [SourceConnector] and hands it to [ListenAndServe]:
//
//	c := &myConnector{...}
//	cfg := &connector.Config{
//	    Port:          "8500",
//	    WriterFactory: factory, // builds a transfer.Writer per batch (gateway-backed)
//	    Sink:          sink,     // optional connector.TimeseriesSink for timeseries bindings
//	}
//	connector.ListenAndServe(ctx, cfg, c)
//
// The server runs one poll loop per binding, serves the internal webhook and
// health endpoints, constructs a fresh [transfer.Writer] for each batch, and
// commits (closes) it for the kit after Sync / HandleWebhook returns — the kit
// never calls Close itself, exactly as in the loader SDK.
//
// # Two emit surfaces
//
// Each Sync / HandleWebhook call receives an [Emitter] bundling both surfaces:
//
//   - Emitter.Entities — a transfer.Writer for vertices/edges. Set a
//     transfer.Vertex.CorrelationKey to request external-ref merge
//     (close-the-loop status sync) instead of create.
//   - Emitter.Timeseries — a TimeseriesSink for fixed-shape measurements
//     (Tier-C timeseries). nil unless a Sink is configured.
//
// A single connector may serve N bindings and use either surface per binding.
//
// # Tenancy
//
// The connector never asserts its own tenant. Tenancy is stamped by the
// platform from the authenticated workload identity (entity commits) or the
// ingress credential (webhook / timeseries intake). The connector only adapts
// the external system and normalizes records.
package connector
