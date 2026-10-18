package egress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func ver(createdAt string, no int64) *EntityVersion {
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		panic(err)
	}
	return &EntityVersion{CreatedAt: t, VersionNo: no}
}

func TestIsStale_TruthTable(t *testing.T) {
	const at = "2026-10-17T05:09:39.832Z"
	const later = "2026-10-18T01:00:00.000Z"
	cases := []struct {
		name             string
		incoming, stored *EntityVersion
		want             bool
	}{
		{"both nil", nil, nil, false},
		{"incoming nil: a row the platform could not stamp is written", nil, ver(at, 7), false},
		{"stored nil: a record written before stamps existed is written over", ver(at, 7), nil, false},
		{"equal is written", ver(at, 7), ver(at, 7), false},
		{"older version_no, same created_at", ver(at, 6), ver(at, 7), true},
		{"newer version_no, same created_at", ver(at, 8), ver(at, 7), false},
		{"re-created entity: later created_at, version_no 1, over a higher number", ver(later, 1), ver(at, 40), false},
		{"predecessor after re-creation: earlier created_at, higher number", ver(at, 40), ver(later, 1), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsStale(c.incoming, c.stored); got != c.want {
				t.Fatalf("IsStale = %v, want %v", got, c.want)
			}
		})
	}
}

// CreatedAt is compared at millisecond precision: the same instant read with a
// different number of fractional digits must not look older.
func TestEntityVersion_CompareIgnoresSubMillisecondDigits(t *testing.T) {
	a := ver("2026-10-17T05:09:39.832Z", 7)
	b := ver("2026-10-17T05:09:39.832999999Z", 7)
	if c := a.Compare(*b); c != 0 {
		t.Fatalf("Compare = %d, want 0 for the same millisecond", c)
	}
	if IsStale(a, b) || IsStale(b, a) {
		t.Fatal("the same millisecond read at two precisions must never be stale")
	}
	// The zone of the parsed value does not matter either.
	c := ver("2026-10-17T13:09:39.832+08:00", 7)
	if a.Compare(*c) != 0 {
		t.Fatal("the same instant in another zone must compare equal")
	}
}

// The wire shape is the one kg-egress-sync D11 specifies:
// "version": {"created_at": "...Z", "version_no": 7}.
func TestEntityVersion_WireShape(t *testing.T) {
	e := Entity{ID: "eq-1", EntityType: "Equipment", Version: ver("2026-10-17T05:09:39.832Z", 7)}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `"version":{"created_at":"2026-10-17T05:09:39.832Z","version_no":7}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("marshalled %s, want it to contain %s", raw, want)
	}

	var back Entity
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Version == nil || back.Version.Compare(*e.Version) != 0 {
		t.Fatalf("round trip lost the stamp: %+v", back.Version)
	}

	// Absent stays absent: a nil stamp is omitted, never sent as zero values,
	// which a component would otherwise read as a real (and very old) version.
	raw, err = json.Marshal(Entity{ID: "eq-2", EntityType: "Equipment"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"version"`) {
		t.Fatalf("a nil stamp must be omitted, got %s", raw)
	}
}

func TestStaleVersionDetail_NamesBothStamps(t *testing.T) {
	got := StaleVersionDetail(*ver("2026-10-17T05:09:39.832Z", 6), *ver("2026-10-17T05:09:39.832Z", 7))
	for _, want := range []string{"2026-10-17T05:09:39.832Z#6", "2026-10-17T05:09:39.832Z#7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail %q does not name %q", got, want)
		}
	}
}

func genVersion(t *rapid.T, label string) EntityVersion {
	// A narrow range of instants so equal CreatedAt values are common, which is
	// where the VersionNo tiebreak matters; sub-millisecond noise is added so the
	// millisecond truncation is exercised.
	ms := rapid.Int64Range(1_760_000_000_000, 1_760_000_000_005).Draw(t, label+"_ms")
	ns := rapid.Int64Range(0, 999_999).Draw(t, label+"_ns")
	return EntityVersion{
		CreatedAt: time.UnixMilli(ms).Add(time.Duration(ns)),
		VersionNo: rapid.Int64Range(1, 6).Draw(t, label+"_no"),
	}
}

// The order is a total order consistent with Compare, and IsStale is exactly
// "strictly before".
func TestEntityVersion_OrderProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a, b, c := genVersion(t, "a"), genVersion(t, "b"), genVersion(t, "c")

		same := a
		if a.Compare(same) != 0 {
			t.Fatal("not reflexive")
		}
		if a.Compare(b) != -b.Compare(a) {
			t.Fatalf("not antisymmetric: %v vs %v", a, b)
		}
		if a.Compare(b) <= 0 && b.Compare(c) <= 0 && a.Compare(c) > 0 {
			t.Fatalf("not transitive: %v %v %v", a, b, c)
		}
		if got, want := IsStale(&a, &b), a.Compare(b) < 0; got != want {
			t.Fatalf("IsStale(%v, %v) = %v, Compare = %d", a, b, got, a.Compare(b))
		}
		if IsStale(&a, &b) && IsStale(&b, &a) {
			t.Fatal("two stamps cannot each be stale against the other")
		}
	})
}

// kg-egress-sync Property 30: a component that applies IsStale ends at the
// greatest version delivered, whatever the arrival order.
func TestEntityVersion_LastWriterIsTheGreatest(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(t, "n")
		deliveries := make([]EntityVersion, n)
		for i := range deliveries {
			deliveries[i] = genVersion(t, "d")
		}
		var held *EntityVersion
		for i := range deliveries {
			d := deliveries[i]
			if IsStale(&d, held) {
				continue
			}
			held = &d
		}
		for _, d := range deliveries {
			if held.Compare(d) < 0 {
				t.Fatalf("held %v is older than delivered %v", held, d)
			}
		}
	})
}
