package connector

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Every Write* method of transfer.Writer must be counted. The list of methods is
// taken from the interface by reflection, not written out here, so a method
// added to transfer.Writer later and not overridden by countingWriter — and so
// promoted, uncounted, from the embedded writer — fails this test rather than
// silently turning batches of that record kind into "nothing emitted".
func TestCountingWriter_CountsEveryWriteMethod(t *testing.T) {
	iface := reflect.TypeOf((*transfer.Writer)(nil)).Elem()
	ctx := reflect.ValueOf(context.Background())
	var checked int
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		if !strings.HasPrefix(m.Name, "Write") {
			continue
		}
		checked++
		t.Run(m.Name, func(t *testing.T) {
			cw := &countingWriter{Writer: transfer.NewNopWriter("")}
			// (ctx, record) — the record's zero value is enough: NopWriter
			// accepts anything, so the only thing under test is the count.
			args := []reflect.Value{ctx, reflect.Zero(m.Type.In(1))}
			out := reflect.ValueOf(cw).MethodByName(m.Name).Call(args)
			if err, _ := out[0].Interface().(error); err != nil {
				t.Fatalf("%s: %v", m.Name, err)
			}
			if cw.n != 1 {
				t.Errorf("%s left the count at %d: countingWriter does not override it, so a batch of only these records would be dropped as empty",
					m.Name, cw.n)
			}
		})
	}
	if checked < 5 {
		t.Fatalf("found %d Write* methods on transfer.Writer; the reflection walk is not seeing the interface", checked)
	}
}

// The case that was broken: a sync that emits only relationship types is a
// non-empty batch and must be committed, not dropped as "nothing emitted".
func TestRunSync_RelationshipTypesOnlyIsCommitted(t *testing.T) {
	dw := &discardingWriter{}
	fc := &fakeConnector{
		bindings: []Binding{{ID: "types"}},
		syncFn: func(ctx context.Context, _ Binding, _ string, em *Emitter) (*SyncResult, error) {
			return &SyncResult{}, em.Entities.WriteRelationshipType(ctx, transfer.RelationshipTypeDef{Name: "feeds"})
		},
	}
	s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return dw, nil })
	if _, err := s.runSync(t.Context(), fc.bindings[0], ""); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if !dw.closed || dw.released {
		t.Errorf("closed=%v released=%v: a relationship-type-only batch must be committed, not discarded", dw.closed, dw.released)
	}
}
