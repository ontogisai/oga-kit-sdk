package loader

import (
	"context"
	"errors"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// SnapshotWriterFactoryOption tunes [NewSnapshotWriterFactory].
type SnapshotWriterFactoryOption func(*snapshotWriterFactoryConfig)

type snapshotWriterFactoryConfig struct {
	governedPredicates []string
}

// resolveSnapshotWriterFactoryConfig applies options once and returns the normalized
// declaration.
//
// Built in ONE place deliberately. An earlier shape had WithCommitClient apply the
// options a second time for its boot-check bookkeeping, which could not diverge
// only because SnapshotWriterFactoryOption is a func over an unexported type and both
// options happened to be stateless appends. A stateful or order-sensitive option
// added later would have made the factory's declaration and the boot check's
// declaration disagree silently, and the warning would then describe a vocabulary
// the factory does not hold.
func resolveSnapshotWriterFactoryConfig(opts []SnapshotWriterFactoryOption) []string {
	cfg := &snapshotWriterFactoryConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return normalizeGovernedPredicates(cfg.governedPredicates)
}

// WithGovernedPredicates declares the predicates this feed OWNS — the vocabulary
// an operator's full-snapshot assertion is allowed to cover.
//
// # Why the kit declares this and the operator does not
//
// Two different questions were being conflated, and they have different owners:
//
//   - "Which predicates does this feed own?" The KIT AUTHOR knows. It is a
//     property of the feed, stable across runs, and belongs in reviewed,
//     versioned source.
//   - "Is this particular import a complete snapshot?" The OPERATOR knows. It is
//     a property of the file they pointed at.
//
// So the kit declares its vocabulary once here, and the operator's per-run
// checkbox asserts only completeness. The operator never has to type a predicate
// name, and the scope the platform acts on is a declaration someone reviewed
// rather than whatever the payload happened to contain.
//
// Declaring a vocabulary NEVER asserts anything on its own: without
// `oga.full_snapshot=true` on the request, no assertion is written and no edge is
// closed. A feed that always asserts (a connector, not an operator-driven import)
// should pass [transfer.WithEdgeCompleteness] directly instead.
//
// # Derive this list, do not retype it
//
// Derive it from whatever already defines the feed's predicate vocabulary (the
// map your predicate normalizer is built on, say) and add a test that fails when
// the two drift. A predicate the feed emits but does not declare here is silently
// ungoverned: its stale edges are never closed. That is the safe direction, and
// the platform now reports it as predicate drift — but it is still a bug.
func WithGovernedPredicates(preds ...string) SnapshotWriterFactoryOption {
	return func(c *snapshotWriterFactoryConfig) {
		c.governedPredicates = append(c.governedPredicates, preds...)
	}
}

// There is deliberately NO option for passing arbitrary [transfer.WriterOption]
// values through to the writer.
//
// An earlier revision had one, and it inverted the whole premise of this factory.
// [transfer.WithEdgeCompleteness] is currently the ONLY exported constructor of a
// transfer.WriterOption, so a pass-through option's sole possible argument was the
// one thing it must never carry: a hard-coded assertion, which arms edge closure on
// EVERY artifact from an entirely empty config, with no operator flag anywhere in
// the request. Verified by probe before removal. A doc comment saying "do not pass
// this" was the only guard, and it cannot be enforced from here — WriterOption is a
// func over a type unexported in transfer, so an option cannot be introspected.
//
// It also removed the last way to hand back a writer that fails at every write: an
// invalid kit-supplied assertion is captured by the writer and surfaces from the
// first Write and from Close, sticky, which silently discards the operator's valid
// assertion applied after it and then fails naming predicates the operator did
// supply.
//
// If a genuinely needed WriterOption appears later, reintroduce a narrow option for
// that specific concern rather than a general pass-through.

// NewSnapshotWriterFactory builds the writer factory every operator-driven loader
// should use.
//
// It replaces the closure each kit used to hand-write, which was byte-identical
// boilerplate everywhere:
//
//	func(_ context.Context, kind transfer.LoadKind, _ *loader.LoadRequest) (transfer.Writer, error) {
//	    return transfer.NewWriter(commitClient, kind, kitID), nil
//	}
//
// The kit contributed only the commit client and the kit id — `kind` already
// arrives from [WithLoaderKind] and the request was discarded — so the seam
// existed for no reason, and every kit that kept it silently opted out of
// anything the chassis later needed to read off the request. Honoring the
// operator's full-snapshot assertion is the first such thing.
//
// On each /load the factory reads the reserved keys off req.Config (see
// [ConfigKeyFullSnapshot]) and, when the operator asserted, stamps
// [transfer.EdgeCompleteness] onto the artifact header. With no assertion the
// writer is byte-identical to the boilerplate one above.
//
// Prefer [WithCommitClient], which installs this in one line.
func NewSnapshotWriterFactory(
	client transfer.CommitClient,
	kitID string,
	opts ...SnapshotWriterFactoryOption,
) WriterFactory {
	return newSnapshotWriterFactory(client, kitID, resolveSnapshotWriterFactoryConfig(opts))
}

// newSnapshotWriterFactory is the shared core, taking the ALREADY-normalized
// declaration so WithCommitClient can resolve the options once and use the same
// value for both the factory and its boot check.
func newSnapshotWriterFactory(
	client transfer.CommitClient,
	kitID string,
	declared []string,
) WriterFactory {
	return func(_ context.Context, kind transfer.LoadKind, req *LoadRequest) (transfer.Writer, error) {
		// A nil commit client is a deployment mistake, not a bad request: it must
		// NOT be reported as 400, and it must not reach transfer.NewWriter, where
		// it would survive construction and panic at Close after the kit had
		// already done the whole load.
		if client == nil {
			return nil, errors.New("loader: snapshot writer factory has no commit client")
		}

		var cfgMap map[string]any
		if req != nil {
			cfgMap = req.Config
		}
		ec, err := assertionFor(cfgMap, kind, declared)
		if err != nil {
			return nil, err
		}

		// Built fresh per request. A factory is called once per /load, so a slice
		// shared across calls would leak one request's assertion into the next —
		// a request that asserted nothing would inherit the previous request's
		// governed predicates and start closing edges.
		if ec == nil {
			return transfer.NewWriter(client, kind, kitID), nil
		}
		return transfer.NewWriter(client, kind, kitID, transfer.WithEdgeCompleteness(*ec)), nil
	}
}

// WithCommitClient is the production wiring for a loader sidecar: it installs the
// snapshot writer factory so the kit never writes a factory closure.
//
//	cfg := &loader.ServerConfig{
//	    Port: port,
//	    HandlerOptions: []loader.HandlerOption{
//	        loader.WithCommitClient(commitClient, kitID,
//	            loader.WithGovernedPredicates(feedPredicates()...)),
//	        loader.WithLoaderKind(transfer.KindData),
//	    },
//	}
//
// [WithWriterFactory] remains available for a loader with genuinely custom writer
// needs (and is what the SDK's own tests use to inject a nop writer), but a kit
// reaching for it opts out of everything the chassis reads off the request.
func WithCommitClient(
	client transfer.CommitClient,
	kitID string,
	opts ...SnapshotWriterFactoryOption,
) HandlerOption {
	// Resolved ONCE and shared, so the factory's declaration and the boot check's
	// declaration cannot drift apart.
	declared := resolveSnapshotWriterFactoryConfig(opts)
	factory := newSnapshotWriterFactory(client, kitID, declared)

	return func(c *handlerConfig) {
		c.writerFactory = factory
		// Recorded so newHandlerConfig can warn about a data loader with no
		// declared vocabulary. It is deliberately checked THERE and not here:
		// the loader kind arrives from a separate option, and warning an ontology
		// loader about a predicate vocabulary it can never use is noise.
		c.readsSnapshotConfig = true
		c.declaredPredicates = declared
	}
}
