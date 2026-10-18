package egress

import (
	"cmp"
	"fmt"
	"time"
)

// EntityVersion identifies which version of an entity a record carries, so a
// component can refuse a write older than the one the external system already
// holds.
//
// Two deliveries of one entity can be in flight at once — a retried batch, a
// full re-run beside a delta, two change flushes — and the older one can land
// last. Without a version the external record then keeps the older value until
// the entity changes again. The platform stamps every entity-lane record and
// every entity withdrawal with the version of the knowledge-graph row it was
// read from; it does not check the stamp itself, because only the component can
// read the external record.
//
// # The order
//
// Versions order by CreatedAt first, then VersionNo. VersionNo alone is not
// enough: the platform restarts it at 1 when an entity is re-created under the
// same id (a kit purge and re-import, or a re-import after the type's storage
// moved), and an external record still holding the old, higher number would
// then refuse every later write to it, permanently. A re-created entity has a
// later CreatedAt, which puts it after every version of its predecessor; for any
// other pair the CreatedAt is equal and VersionNo decides.
//
// CreatedAt is compared at MILLISECOND precision. The platform stores it at
// millisecond precision, and a component that keeps the stamp in an external
// system may read it back with a different number of fractional digits; neither
// may make an equal instant compare older.
//
// # What a component does with it
//
//   - Record the stamp it applied with the external record (in the record's own
//     metadata is the usual place), so the component stays stateless.
//   - Before an update, read the stored stamp and refuse the write when
//     [IsStale] reports it older: answer [Batch.SkippedReason] with
//     [ReasonStaleVersion] and [StaleVersionDetail]. A stale write is a
//     success, not a failure — a newer write is already there.
//   - Hold a lock per entity id across that read, compare and write. Two
//     requests for one entity can be in flight together, and an external system
//     without a conditional update cannot make the pair atomic.
//   - Write an EQUAL stamp. Fields that do not advance the version (owner
//     references after a move, [TypeRefKey], [Entity.TypeAncestry]) reach the
//     external system only by re-pushing the same version, and a full run sends
//     equal versions throughout.
//   - Write when either stamp is MISSING. A record written before stamps existed
//     holds none, and a row the platform could not stamp carries none; neither
//     may block a write.
//   - On a withdrawal, record the tombstone's stamp with the retired record, so a
//     stale push still in flight cannot revive it.
//
// The stamp orders deliveries that ARRIVE. It does not recover a change the
// platform never delivered.
type EntityVersion struct {
	// CreatedAt is when the entity was created in the knowledge graph (RFC3339,
	// UTC on the wire). It is not rewritten by an update.
	CreatedAt time.Time `json:"created_at"`

	// VersionNo is the entity's version number. It advances on every change the
	// platform records and restarts at 1 when the entity is re-created.
	VersionNo int64 `json:"version_no"`
}

// Compare orders v against o: -1 when v is older, 0 when equal, +1 when newer.
// CreatedAt is compared at millisecond precision, then VersionNo.
func (v EntityVersion) Compare(o EntityVersion) int {
	if c := cmp.Compare(v.CreatedAt.UnixMilli(), o.CreatedAt.UnixMilli()); c != 0 {
		return c
	}
	return cmp.Compare(v.VersionNo, o.VersionNo)
}

// String renders the stamp for a reason detail or a log line:
// "2026-10-17T05:09:39.832Z#7".
func (v EntityVersion) String() string {
	return fmt.Sprintf("%s#%d", v.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"), v.VersionNo)
}

// IsStale reports whether a write carrying incoming must be refused because the
// external record already holds stored.
//
// True only when BOTH stamps are present and incoming orders strictly before
// stored. An equal stamp is not stale (see [EntityVersion] for why it must be
// written), and a missing stamp on either side is never stale.
func IsStale(incoming, stored *EntityVersion) bool {
	if incoming == nil || stored == nil {
		return false
	}
	return incoming.Compare(*stored) < 0
}

// StaleVersionDetail is the reason detail for a write refused as stale, naming
// both stamps. Use it with [ReasonStaleVersion] so every component words the
// refusal the same way:
//
//	if egress.IsStale(e.Version, stored) {
//	    b.SkippedReason(e.ID, egress.ReasonStaleVersion, egress.StaleVersionDetail(*e.Version, *stored))
//	    continue
//	}
func StaleVersionDetail(incoming, stored EntityVersion) string {
	return fmt.Sprintf("incoming version %s is older than the version %s the external record holds",
		incoming, stored)
}

// ReasonStaleVersion is the reason code for a write a component refused because
// the external record already holds a newer version (see [IsStale]). It is a
// skip, so it lands in the run report's skip tally and never reads as a
// failure.
//
// Declared here, in the contract package, so every component reports the same
// byte-for-byte code and a receiver of the outcome report can match it.
const ReasonStaleVersion = "stale_version"
