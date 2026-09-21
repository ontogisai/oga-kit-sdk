package loader

import (
	"context"
	"errors"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// ErrInvalidLoadConfig marks a /load failure caused by the REQUEST rather than by
// the loader or the platform, so [ServeLoader] answers 400 instead of 500.
//
// Wrap it from a custom [WithWriterFactory] factory that rejects a kit-defined
// config value. Both cases used to be 500, which made a malformed caller-supplied
// config read as a platform fault and get triaged as one, when the remedy is to fix
// the request.
//
// Nothing in the SDK produces it any more: the only producer was the reserved
// `oga.*` snapshot-config parsing, removed in OGA-930 when the platform stopped
// reading the assertion off the artifact. It is retained because the 400-vs-500
// distinction is part of the kit-facing contract, not a detail of that one feature —
// a kit's own factory is the remaining legitimate producer.
var ErrInvalidLoadConfig = errors.New("invalid load config")

// WithCommitClient is the production wiring for a loader sidecar: it installs the
// standard writer factory so the kit never hand-writes a factory closure.
//
//	cfg := &loader.ServerConfig{
//	    Port: port,
//	    HandlerOptions: []loader.HandlerOption{
//	        loader.WithCommitClient(commitClient, kitID),
//	        loader.WithLoaderKind(transfer.KindData),
//	    },
//	}
//
// It replaces the closure every kit used to write, which was byte-identical
// boilerplate everywhere:
//
//	func(_ context.Context, kind transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
//	    return transfer.NewWriter(commitClient, kind, kitID), nil
//	}
//
// The kit contributed only the commit client and the kit id — `kind` already arrives
// from [WithLoaderKind] and the request was discarded — so the seam existed for no
// reason.
//
// [WithWriterFactory] remains available for a loader with genuinely custom writer
// needs (and is what the SDK's own tests use to inject a nop writer).
//
// # Edge completeness is not configured here
//
// An earlier version took a variadic of options, the only one being
// WithGovernedPredicates, and reflected a reserved `oga.full_snapshot` config key
// into the artifact header. All of that is gone (OGA-930): the platform resolves the
// assertion from its OWN state, keyed on the gateway-verified identity of the sidecar
// that submits, so nothing about it travels through the loader. Declare completeness
// in the kit manifest — `loaders[].governed_predicates_file` for an operator-driven
// import, `source_connectors[].edge_completeness` for a continuous feed.
func WithCommitClient(client transfer.CommitClient, kitID string) HandlerOption {
	factory := newWriterFactory(client, kitID)
	return func(c *handlerConfig) {
		c.writerFactory = factory
	}
}

// newWriterFactory builds the standard per-request writer.
//
// Unexported: [WithCommitClient] is the only way in. The exported
// NewSnapshotWriterFactory it replaces had no callers outside the SDK's own tests,
// and its name promised snapshot semantics this no longer has.
func newWriterFactory(client transfer.CommitClient, kitID string) WriterFactory {
	return func(_ context.Context, kind transfer.LoadKind, _ *LoadRequest) (transfer.Writer, error) {
		// A nil commit client is a deployment mistake, not a bad request: it must
		// NOT be reported as 400, and it must not reach transfer.NewWriter, where it
		// would survive construction and panic at Close after the kit had already
		// done the whole load.
		if client == nil {
			return nil, errors.New("loader: writer factory has no commit client")
		}
		return transfer.NewWriter(client, kind, kitID), nil
	}
}
