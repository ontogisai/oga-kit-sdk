package loader

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// The reserved key names are a CONTRACT with the platform, which writes them in
// internal/admin/data_import_snapshot.go. A rename on either side makes the
// operator's assertion silently unreadable — the import succeeds and closes
// nothing, which looks exactly like the feature being broken. Pin the literals.
func TestReservedConfigKeys_Literals(t *testing.T) {
	t.Parallel()
	if ConfigKeyFullSnapshot != "oga.full_snapshot" {
		t.Errorf("ConfigKeyFullSnapshot = %q, want oga.full_snapshot", ConfigKeyFullSnapshot)
	}
	if ConfigKeyGovernedPredicates != "oga.governed_predicates" {
		t.Errorf("ConfigKeyGovernedPredicates = %q, want oga.governed_predicates", ConfigKeyGovernedPredicates)
	}
}

func TestParseSnapshotAssertion_Absent(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]map[string]any{
		"nil map":   nil,
		"empty map": {},
		"kit keys only": {
			"batch_size": 100,
			"strict":     true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			asserted, preds, err := parseSnapshotAssertion(cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if asserted {
				t.Error("asserted = true with no reserved keys")
			}
			if preds != nil {
				t.Errorf("predicates = %v, want nil", preds)
			}
		})
	}
}

func TestParseSnapshotAssertion_Asserted(t *testing.T) {
	t.Parallel()
	asserted, preds, err := parseSnapshotAssertion(map[string]any{
		ConfigKeyFullSnapshot:       true,
		ConfigKeyGovernedPredicates: []any{"feeds", "hasPoint"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !asserted {
		t.Fatal("asserted = false")
	}
	if want := []string{"feeds", "hasPoint"}; !reflect.DeepEqual(preds, want) {
		t.Errorf("predicates = %v, want %v", preds, want)
	}
}

// The flag must be a real JSON bool. A quoted "true" is the shape a hand-written
// curl or a loosely-typed client sends, and coercing it would mean a
// truthy-looking value silently arms edge closure.
func TestParseSnapshotAssertion_RefusesNonBoolFlag(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]any{
		`string "true"`:  "true",
		`string "false"`: "false",
		"number 1":       float64(1),
		"nil":            nil,
		"list":           []any{true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseSnapshotAssertion(map[string]any{ConfigKeyFullSnapshot: v})
			if !errors.Is(err, ErrInvalidLoadConfig) {
				t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
			}
			if !strings.Contains(err.Error(), "JSON boolean") {
				t.Errorf("error should name the expected type, got: %v", err)
			}
		})
	}
}

// Predicates without a true assertion are refused, not dropped. Dropping them
// would leave the caller believing they had scoped an assertion they never made.
func TestParseSnapshotAssertion_RefusesPredicatesWithoutAssertion(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"flag absent": {
			ConfigKeyGovernedPredicates: []any{"feeds"},
		},
		"flag false": {
			ConfigKeyFullSnapshot:       false,
			ConfigKeyGovernedPredicates: []any{"feeds"},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseSnapshotAssertion(cfg)
			if !errors.Is(err, ErrInvalidLoadConfig) {
				t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
			}
		})
	}
}

// flag=false on its own is a legitimate "not a snapshot" and must be silent.
func TestParseSnapshotAssertion_ExplicitFalseIsNotAnError(t *testing.T) {
	t.Parallel()
	asserted, preds, err := parseSnapshotAssertion(map[string]any{ConfigKeyFullSnapshot: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if asserted || preds != nil {
		t.Errorf("asserted=%v preds=%v, want false/nil", asserted, preds)
	}
}

func TestParseGovernedPredicates_Shapes(t *testing.T) {
	t.Parallel()
	t.Run("[]any of string (the JSON decode shape)", func(t *testing.T) {
		t.Parallel()
		got, has, err := parseGovernedPredicates(map[string]any{
			ConfigKeyGovernedPredicates: []any{"a", "b"},
		})
		if err != nil || !has || !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("got=%v has=%v err=%v", got, has, err)
		}
	})
	t.Run("[]string (a Go caller building the map)", func(t *testing.T) {
		t.Parallel()
		got, has, err := parseGovernedPredicates(map[string]any{
			ConfigKeyGovernedPredicates: []string{"a", "b"},
		})
		if err != nil || !has || !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("got=%v has=%v err=%v", got, has, err)
		}
	})
	t.Run("non-string element is refused, not skipped", func(t *testing.T) {
		t.Parallel()
		_, _, err := parseGovernedPredicates(map[string]any{
			ConfigKeyGovernedPredicates: []any{"a", 7, "b"},
		})
		if !errors.Is(err, ErrInvalidLoadConfig) {
			t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
		}
		// The index matters: a long list needs to say WHICH element is wrong.
		if !strings.Contains(err.Error(), "[1]") {
			t.Errorf("error should name the offending index, got: %v", err)
		}
	})
	t.Run("a bare string is not a list", func(t *testing.T) {
		t.Parallel()
		_, _, err := parseGovernedPredicates(map[string]any{
			ConfigKeyGovernedPredicates: "feeds",
		})
		if !errors.Is(err, ErrInvalidLoadConfig) {
			t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
		}
	})
	t.Run("a list that trims to nothing reads as absent", func(t *testing.T) {
		t.Parallel()
		got, has, err := parseGovernedPredicates(map[string]any{
			ConfigKeyGovernedPredicates: []any{"", "   "},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Emptiness is the contract, not nil-ness — see
		// TestNormalizeGovernedPredicates_MatchesPlatformSemantics.
		if has || len(got) != 0 {
			t.Fatalf("got=%v has=%v, want empty/false", got, has)
		}
	})
}

// This normalization must stay identical to the platform's own. The two ends
// compare governed predicates against live edge rows, so a difference would make
// an artifact govern a predicate the platform does not, with neither side
// reporting anything.
func TestNormalizeGovernedPredicates_MatchesPlatformSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"trims", []string{"  feeds  "}, []string{"feeds"}},
		{"drops blanks", []string{"feeds", "", "  ", "hasPoint"}, []string{"feeds", "hasPoint"}},
		{"de-duplicates", []string{"feeds", "feeds"}, []string{"feeds"}},
		{"de-duplicates after trimming", []string{"feeds", " feeds "}, []string{"feeds"}},
		{"preserves first-seen order", []string{"c", "a", "b", "a"}, []string{"c", "a", "b"}},
		{"all blank collapses to nil", []string{"", " "}, nil},
		{"case is significant", []string{"Feeds", "feeds"}, []string{"Feeds", "feeds"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeGovernedPredicates(tc.in)
			// nil versus empty-non-nil is deliberately NOT asserted. Verified
			// against the platform implementation: it returns nil only for an
			// EMPTY input, and empty-non-nil when every element trims away. The
			// distinction carries no meaning here — every consumer on both sides
			// tests len(), and the wire field is `omitempty`, so nil and empty
			// serialize identically. Asserting nil-ness would have forced the SDK
			// to diverge from the platform to satisfy the test, which is the one
			// thing this test exists to prevent.
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Errorf("normalize(%v) = %v, want empty", tc.in, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalize(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAssertionFor_NoAssertionYieldsNil(t *testing.T) {
	t.Parallel()
	ec, err := assertionFor(nil, transfer.KindData, []string{"feeds"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ec != nil {
		t.Fatalf("assertion = %+v, want nil (a declared vocabulary must not assert on its own)", ec)
	}
}

func TestAssertionFor_FallsBackToDeclaredVocabulary(t *testing.T) {
	t.Parallel()
	ec, err := assertionFor(
		map[string]any{ConfigKeyFullSnapshot: true},
		transfer.KindData,
		[]string{" feeds ", "hasPoint", "feeds"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ec == nil {
		t.Fatal("assertion = nil")
	}
	if ec.Mode != transfer.EdgeCompletenessPerSource {
		t.Errorf("mode = %q, want per_source", ec.Mode)
	}
	// The declared list is normalized too — a kit's slice is no more trustworthy
	// than an operator's.
	if want := []string{"feeds", "hasPoint"}; !reflect.DeepEqual(ec.GovernedPredicates, want) {
		t.Errorf("predicates = %v, want %v", ec.GovernedPredicates, want)
	}
}

func TestAssertionFor_OperatorOverrideWins(t *testing.T) {
	t.Parallel()
	ec, err := assertionFor(
		map[string]any{
			ConfigKeyFullSnapshot:       true,
			ConfigKeyGovernedPredicates: []any{"hasPoint"},
		},
		transfer.KindData,
		[]string{"feeds", "hasPoint", "hasPart"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"hasPoint"}; !reflect.DeepEqual(ec.GovernedPredicates, want) {
		t.Fatalf("predicates = %v, want %v (the operator scopes tighter than the kit)",
			ec.GovernedPredicates, want)
	}
}

// An operator asserted but there is nothing to govern. Refuse, and name BOTH
// remedies: the kit declaration (the real fix) and the per-run override (the
// immediate workaround).
func TestAssertionFor_RefusesWhenNoVocabularyIsAvailable(t *testing.T) {
	t.Parallel()
	_, err := assertionFor(map[string]any{ConfigKeyFullSnapshot: true}, transfer.KindData, nil)
	if !errors.Is(err, ErrInvalidLoadConfig) {
		t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
	}
	for _, want := range []string{"WithGovernedPredicates", ConfigKeyGovernedPredicates} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// A declared list of nothing but blanks is the same as no declaration. Without
// this the empty set would reach transfer.Validate and be refused with a less
// actionable message.
func TestAssertionFor_BlankDeclarationIsNoDeclaration(t *testing.T) {
	t.Parallel()
	_, err := assertionFor(
		map[string]any{ConfigKeyFullSnapshot: true},
		transfer.KindData,
		[]string{"", "   "},
	)
	if !errors.Is(err, ErrInvalidLoadConfig) {
		t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
	}
	if !strings.Contains(err.Error(), "WithGovernedPredicates") {
		t.Errorf("error should name the missing declaration, got: %v", err)
	}
}

// Refused at the factory, not deferred to the first write. The writer rejects it
// too (defense in depth), but there the failure surfaces mid-stream after the
// import has already started.
func TestAssertionFor_RefusesOnOntologyKind(t *testing.T) {
	t.Parallel()
	_, err := assertionFor(
		map[string]any{ConfigKeyFullSnapshot: true},
		transfer.KindOntology,
		[]string{"feeds"},
	)
	if !errors.Is(err, ErrInvalidLoadConfig) {
		t.Fatalf("err = %v, want ErrInvalidLoadConfig", err)
	}
	if !strings.Contains(err.Error(), "ontology") {
		t.Errorf("error should name the reason, got: %v", err)
	}
}

// An ontology loader with no assertion is ordinary and must not be disturbed.
func TestAssertionFor_OntologyWithoutAssertionIsFine(t *testing.T) {
	t.Parallel()
	ec, err := assertionFor(map[string]any{}, transfer.KindOntology, []string{"feeds"})
	if err != nil || ec != nil {
		t.Fatalf("ec=%+v err=%v, want nil/nil", ec, err)
	}
}

// --- the boot-time vocabulary warning ------------------------------------

// A data loader that declares no vocabulary refuses every operator assertion.
// That is correct, but it must not be silent, or a forgotten declaration looks
// exactly like a broken platform feature.
func TestWarnIfNoGovernedPredicates(t *testing.T) {
	t.Parallel()

	capture := func(c *handlerConfig) string {
		var buf bytes.Buffer
		lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		warnIfNoGovernedPredicates(c, lg)
		return buf.String()
	}

	t.Run("data loader with no declaration warns", func(t *testing.T) {
		t.Parallel()
		out := capture(&handlerConfig{
			standardWriterInstalled: true,
			kind:                    transfer.KindData,
		})
		if !strings.Contains(out, "no governed predicates declared") {
			t.Fatalf("expected a warning, got %q", out)
		}
		// The remedy has to be in the line: a warning that only states the
		// symptom leaves the reader to go find the option name.
		if !strings.Contains(out, "WithGovernedPredicates") {
			t.Errorf("warning should name the remedy, got %q", out)
		}
	})

	t.Run("data loader with a declaration is silent", func(t *testing.T) {
		t.Parallel()
		out := capture(&handlerConfig{
			standardWriterInstalled: true,
			kind:                    transfer.KindData,
			declaredPredicates:      []string{"feeds"},
		})
		if out != "" {
			t.Fatalf("expected silence, got %q", out)
		}
	})

	// An ontology loader can never carry an assertion, so the same warning there
	// is noise that trains operators to ignore it.
	t.Run("ontology loader is silent", func(t *testing.T) {
		t.Parallel()
		out := capture(&handlerConfig{
			standardWriterInstalled: true,
			kind:                    transfer.KindOntology,
		})
		if out != "" {
			t.Fatalf("expected silence for an ontology loader, got %q", out)
		}
	})

	// A kit on the advanced path (WithWriterFactory) opted out of the chassis
	// reading the request at all; warning it about a vocabulary it never offered
	// to declare would be wrong.
	t.Run("hand-rolled factory is silent", func(t *testing.T) {
		t.Parallel()
		out := capture(&handlerConfig{kind: transfer.KindData})
		if out != "" {
			t.Fatalf("expected silence for a hand-rolled factory, got %q", out)
		}
	})

	t.Run("nil inputs are tolerated", func(t *testing.T) {
		t.Parallel()
		warnIfNoGovernedPredicates(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		warnIfNoGovernedPredicates(&handlerConfig{}, nil)
	})
}

// WithCommitClient must record BOTH the factory and the declaration state, or the
// warning above can never fire.
func TestWithCommitClient_RecordsDeclarationForTheBootCheck(t *testing.T) {
	t.Parallel()
	c := &handlerConfig{}
	WithCommitClient(&transfer.FakeCommitClient{}, "example-kit",
		WithGovernedPredicates(" feeds ", "feeds", "hasPoint"))(c)

	if !c.standardWriterInstalled {
		t.Error("standardWriterInstalled not set")
	}
	if c.writerFactory == nil {
		t.Error("writerFactory not set")
	}
	// Normalized at record time so the boot check cannot be fooled by a
	// whitespace-only declaration.
	if want := []string{"feeds", "hasPoint"}; !reflect.DeepEqual(c.declaredPredicates, want) {
		t.Errorf("declaredPredicates = %v, want %v", c.declaredPredicates, want)
	}
}
