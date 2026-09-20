package loader

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Reserved loader-config keys carrying an OPERATOR's per-run full-snapshot
// assertion (OGA-928, contract defined by OGA-914).
//
// # Why the operator's intent travels in `config`
//
// The edge-completeness assertion rides the ARTIFACT HEADER, which the loader
// writes. For a connector that is the right place — its author knows whether the
// feed emits complete snapshots. But a loader driven by an operator data import
// handles artifacts that are sometimes complete and sometimes partial, and only
// the person who chose the source file knows which. The platform cannot tell a
// partial artifact from a shrunken one, and it cannot correlate an artifact back
// to the import request either: `loader.complete` mints its own job id and the
// transfer commit carries no import-job handle.
//
// So the intent travels THROUGH the loader. The platform puts it in the `config`
// map that DataImportRequest.Config → DataImportInput.Config → SubmitLoad already
// forwards to POST /load, and [NewStandardWriterFactory] reflects it into the
// header.
//
// # Namespacing
//
// The `oga.` prefix keeps these out of a kit's own config namespace: `config` is
// otherwise entirely kit-defined, so an unprefixed `full_snapshot` could collide
// with a key a kit already reads.
const (
	// ConfigKeyFullSnapshot is a bool. True means the operator asserts this
	// artifact is the COMPLETE edge set for the assets it carries, so a
	// relationship it omits has ended.
	//
	// Only a JSON bool is accepted. The string "true" is REFUSED — see
	// [parseSnapshotAssertion].
	ConfigKeyFullSnapshot = "oga.full_snapshot"

	// ConfigKeyGovernedPredicates is a list of strings naming the predicates the
	// assertion covers. OPTIONAL: when absent the kit's own declared vocabulary
	// ([WithGovernedPredicates]) is used, so an operator never has to type a
	// predicate name.
	//
	// Supply it to scope TIGHTER than the kit's declaration, or to converge a
	// removal-to-empty for a predicate the kit no longer emits at all (that
	// predicate is absent from the artifact, so nothing else can name it).
	ConfigKeyGovernedPredicates = "oga.governed_predicates"
)

// ErrInvalidLoadConfig marks a /load failure caused by the REQUEST rather than by
// the loader or the platform. [Handler] maps it to HTTP 400.
//
// It exists because the writer factory's error was previously reported as 500
// unconditionally, and a malformed operator-supplied config key returned as 500
// reads as a platform fault — it would be triaged as one, while the actual remedy
// is to fix the request.
var ErrInvalidLoadConfig = errors.New("invalid load config")

// invalidConfigf builds an [ErrInvalidLoadConfig]-wrapped error.
func invalidConfigf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidLoadConfig, fmt.Sprintf(format, args...))
}

// parseSnapshotAssertion reads the reserved keys off a /load config map.
//
// Returns asserted=false when the operator did not assert, in which case the
// returned predicate list is always nil and the caller must add NOTHING to the
// header — a request without the reserved keys has to produce a byte-identical
// artifact to one from before this feature existed.
//
// # Fail closed, never coerce
//
// Every rejection below is deliberate. This value LICENSES THE PLATFORM TO CLOSE
// LIVE EDGES, so a guess about what the caller meant is not an acceptable
// substitute for an error:
//
//   - A non-bool `oga.full_snapshot` is refused, including the string "true".
//     Accepting "true" would mean any truthy-looking value silently arms edge
//     closure, and a caller that sent a string did not send what the contract
//     asks for.
//   - Predicates supplied WITHOUT a true assertion are refused, whether the flag
//     is absent or explicitly false. Silently dropping the list would leave the
//     caller believing they had scoped an assertion they never made, and the
//     visible result (nothing converges) is indistinguishable from the feature
//     being broken. This mirrors [transfer.EdgeCompleteness.Validate], which
//     rejects the same shape.
//   - A non-string element in the list is refused rather than skipped, for the
//     same reason: a partially-read list is a scope nobody chose.
//
// # Non-goal: deriving the governed set
//
// The governed list is NEVER derived from the predicates the artifact happens to
// carry, and that must not be added as a convenience. Deriving it makes the
// governed scope FOLLOW THE PAYLOAD: a feed that one day widens its vocabulary
// would silently widen what the platform is licensed to close, with no review
// anywhere. The list exists precisely to be a declaration someone reviewed.
//
// (It is also not reachable here. The header is written lazily on the writer's
// FIRST record, so the full predicate set is not known by the time the header is
// serialized.)
func parseSnapshotAssertion(cfg map[string]any) (asserted bool, predicates []string, err error) {
	rawFlag, hasFlag := cfg[ConfigKeyFullSnapshot]

	preds, hasPreds, err := parseGovernedPredicates(cfg)
	if err != nil {
		return false, nil, err
	}

	if !hasFlag {
		if hasPreds {
			return false, nil, invalidConfigf(
				"%s was supplied without %s=true; set the assertion or drop the predicate list",
				ConfigKeyGovernedPredicates, ConfigKeyFullSnapshot)
		}
		return false, nil, nil
	}

	flag, ok := rawFlag.(bool)
	if !ok {
		return false, nil, invalidConfigf(
			"%s must be a JSON boolean, got %T (a quoted \"true\" is not accepted)",
			ConfigKeyFullSnapshot, rawFlag)
	}
	if !flag {
		if hasPreds {
			return false, nil, invalidConfigf(
				"%s was supplied with %s=false; set the assertion or drop the predicate list",
				ConfigKeyGovernedPredicates, ConfigKeyFullSnapshot)
		}
		return false, nil, nil
	}
	return true, preds, nil
}

// parseGovernedPredicates reads and normalizes the predicate list.
//
// hasPreds reports whether a NON-EMPTY list survived normalization. A list that
// trims to nothing is reported as absent rather than as an empty governed set:
// an empty set is refused by [transfer.EdgeCompleteness.Validate] anyway, and
// treating it as absent lets the kit's declared vocabulary apply, which is what
// the caller who sent whitespace plainly wanted.
func parseGovernedPredicates(cfg map[string]any) (preds []string, hasPreds bool, err error) {
	raw, ok := cfg[ConfigKeyGovernedPredicates]
	if !ok {
		return nil, false, nil
	}

	var in []string
	switch v := raw.(type) {
	case []string:
		// A Go caller building the map directly (a test, or a platform-side
		// helper that never round-tripped through JSON).
		in = v
	case []any:
		// The JSON decode shape, which is what actually arrives over /load.
		in = make([]string, 0, len(v))
		for i, elem := range v {
			s, isStr := elem.(string)
			if !isStr {
				return nil, false, invalidConfigf(
					"%s[%d] must be a string, got %T", ConfigKeyGovernedPredicates, i, elem)
			}
			in = append(in, s)
		}
	default:
		return nil, false, invalidConfigf(
			"%s must be a list of strings, got %T", ConfigKeyGovernedPredicates, raw)
	}

	out := normalizeGovernedPredicates(in)
	return out, len(out) > 0, nil
}

// normalizeGovernedPredicates trims, drops blanks and de-duplicates while
// preserving first-seen order.
//
// This MUST stay behaviorally identical to the platform's own
// normalizeGovernedPredicates (internal/admin/data_import_snapshot.go). The two
// ends compare governed predicates against live edge rows, so a normalization
// difference would make an artifact govern a predicate the platform does not (or
// the reverse) without either side reporting anything.
func normalizeGovernedPredicates(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// assertionFor resolves the header assertion for one /load request, or reports
// that none is asserted.
//
// Precedence: an operator-supplied list WINS over the kit's declared vocabulary.
// On the data-import path the operator is the authority — they may legitimately
// need to scope tighter than the kit's declaration for a particular run.
func assertionFor(
	cfg map[string]any,
	kind transfer.LoadKind,
	declared []string,
) (*transfer.EdgeCompleteness, error) {
	asserted, operatorPreds, err := parseSnapshotAssertion(cfg)
	if err != nil {
		return nil, err
	}
	if !asserted {
		return nil, nil
	}

	// Refuse at the factory rather than letting the writer defer the same
	// rejection to the first write: an ontology artifact carries no edge
	// instances to scope, so the request is wrong and should fail now.
	if kind == transfer.KindOntology {
		return nil, invalidConfigf(
			"%s is not valid for an ontology loader (an ontology artifact carries no edge instances to scope)",
			ConfigKeyFullSnapshot)
	}

	preds := operatorPreds
	if len(preds) == 0 {
		preds = normalizeGovernedPredicates(declared)
	}
	if len(preds) == 0 {
		return nil, invalidConfigf(
			"%s=true but no governed predicates are available: this loader declares none "+
				"(see loader.WithGovernedPredicates) and the request supplied no %s",
			ConfigKeyFullSnapshot, ConfigKeyGovernedPredicates)
	}

	ec := transfer.EdgeCompleteness{
		Mode:               transfer.EdgeCompletenessPerSource,
		GovernedPredicates: preds,
	}
	// Validate BEFORE the writer is built. transfer.WithEdgeCompleteness has no
	// error to return at construction, so it captures the failure and surfaces it
	// from the first Write and from Close — correct for a kit-author mistake, but
	// for an operator-supplied value it would appear as an opaque mid-stream
	// failure after the import had already started.
	if err := ec.Validate(); err != nil {
		return nil, invalidConfigf("%s", err)
	}
	return &ec, nil
}
