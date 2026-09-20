package loader

import (
	"context"
	"errors"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// StandardWriterOption tunes [NewStandardWriterFactory].
type StandardWriterOption func(*standardWriterConfig)

type standardWriterConfig struct {
	governedPredicates []string
	writerOptions      []transfer.WriterOption
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
func WithGovernedPredicates(preds ...string) StandardWriterOption {
	return func(c *standardWriterConfig) {
		c.governedPredicates = append(c.governedPredicates, preds...)
	}
}

// WithWriterOptions passes [transfer.WriterOption] values through to every writer
// the factory builds.
//
// Do NOT pass [transfer.WithEdgeCompleteness] here. On an operator-driven import
// the assertion is the operator's per-run decision, resolved from the request; a
// hard-coded one would assert completeness for a partial import too, which is how
// live edges get closed by mistake.
func WithWriterOptions(opts ...transfer.WriterOption) StandardWriterOption {
	return func(c *standardWriterConfig) {
		c.writerOptions = append(c.writerOptions, opts...)
	}
}

// NewStandardWriterFactory builds the writer factory every operator-driven loader
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
func NewStandardWriterFactory(
	client transfer.CommitClient,
	kitID string,
	opts ...StandardWriterOption,
) WriterFactory {
	cfg := &standardWriterConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	declared := normalizeGovernedPredicates(cfg.governedPredicates)

	return func(_ context.Context, kind transfer.LoadKind, req *LoadRequest) (transfer.Writer, error) {
		// A nil commit client is a deployment mistake, not a bad request: it must
		// NOT be reported as 400, and it must not reach transfer.NewWriter, where
		// it would survive construction and panic at Close after the kit had
		// already done the whole load.
		if client == nil {
			return nil, errors.New("loader: standard writer factory has no commit client")
		}

		var cfgMap map[string]any
		if req != nil {
			cfgMap = req.Config
		}
		ec, err := assertionFor(cfgMap, kind, declared)
		if err != nil {
			return nil, err
		}

		// Copy per request. The option slice is appended to below, and a factory
		// is called once per /load — appending to the shared slice would leak one
		// request's assertion into the next.
		wopts := make([]transfer.WriterOption, 0, len(cfg.writerOptions)+1)
		wopts = append(wopts, cfg.writerOptions...)
		if ec != nil {
			wopts = append(wopts, transfer.WithEdgeCompleteness(*ec))
		}
		return transfer.NewWriter(client, kind, kitID, wopts...), nil
	}
}

// WithCommitClient is the production wiring for a loader sidecar: it installs the
// standard writer factory so the kit never writes a factory closure.
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
	opts ...StandardWriterOption,
) HandlerOption {
	factory := NewStandardWriterFactory(client, kitID, opts...)
	cfg := &standardWriterConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	declared := normalizeGovernedPredicates(cfg.governedPredicates)

	return func(c *handlerConfig) {
		c.writerFactory = factory
		// Recorded so newHandlerConfig can warn about a data loader with no
		// declared vocabulary. It is deliberately checked THERE and not here:
		// the loader kind arrives from a separate option, and warning an ontology
		// loader about a predicate vocabulary it can never use is noise.
		c.standardWriterInstalled = true
		c.declaredPredicates = declared
	}
}
