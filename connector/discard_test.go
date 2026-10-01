package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// discardingWriter is a fakeWriter that also implements transfer.Discarder
// with the interface's contract (a no-op once closed), and records which of the
// two ended it.
type discardingWriter struct {
	fakeWriter
	discardCalls int
	released     bool // Discard ran before any Close
}

func (d *discardingWriter) Discard() error {
	d.discardCalls++
	if !d.closed {
		d.released = true
	}
	return nil
}

var _ transfer.Discarder = (*discardingWriter)(nil)

// Every way a server path can end, for each of the three paths that build a
// writer. A dropped writer must be discarded; a committed one must be closed
// and not discarded first. The Discard is asserted on the FACTORY's writer —
// the server wraps it in countingWriter, which would hide the method.
func TestServer_DiscardsEveryUncommittedWriter(t *testing.T) {
	type outcome int
	const (
		handlerError outcome = iota
		nothingEmitted
		committed
		commitFails
	)
	emit := func(ctx context.Context, em *Emitter, o outcome) error {
		if o == nothingEmitted {
			return nil
		}
		_ = em.Entities.WriteVertex(ctx, transfer.Vertex{EntityType: "WorkOrder"})
		if o == handlerError {
			return errors.New("source unreachable")
		}
		return nil
	}
	paths := []struct {
		name string
		run  func(t *testing.T, s *server, b Binding)
	}{
		{"runSync", func(t *testing.T, s *server, b Binding) { _, _ = s.runSync(t.Context(), b, "") }},
		{"processWebhookSync", func(t *testing.T, s *server, b Binding) {
			r := httptest.NewRequest(http.MethodPost, "/webhook/"+b.ID, nil)
			s.processWebhookSync(httptest.NewRecorder(), r, b, []byte(`{}`))
		}},
		{"processWebhookAsync", func(t *testing.T, s *server, b Binding) {
			s.processWebhookAsync(t.Context(), webhookJob{binding: b, payload: []byte(`{}`)})
		}},
	}
	for _, p := range paths {
		for _, o := range []struct {
			name        string
			outcome     outcome
			wantClosed  bool
			wantRelease bool
		}{
			{"handler error", handlerError, false, true},
			{"nothing emitted", nothingEmitted, false, true},
			{"committed", committed, true, false},
			{"commit fails", commitFails, true, false}, // Close ended it; it released its own body
		} {
			t.Run(p.name+"/"+o.name, func(t *testing.T) {
				dw := &discardingWriter{}
				if o.outcome == commitFails {
					dw.closeErr = errors.New("gateway down")
				}
				b := Binding{ID: "wo", Mode: ModeWebhook}
				fc := &fakeConnector{
					bindings: []Binding{b},
					syncFn: func(ctx context.Context, _ Binding, _ string, em *Emitter) (*SyncResult, error) {
						if err := emit(ctx, em, o.outcome); err != nil {
							return nil, err
						}
						return &SyncResult{}, nil
					},
					webhookFn: func(ctx context.Context, _ Binding, _ []byte, em *Emitter) error {
						return emit(ctx, em, o.outcome)
					},
				}
				s := newTestServer(fc, func(context.Context, Binding) (transfer.Writer, error) { return dw, nil })
				p.run(t, s, b)

				if dw.closed != o.wantClosed {
					t.Errorf("closed = %v, want %v", dw.closed, o.wantClosed)
				}
				if dw.released != o.wantRelease {
					t.Errorf("released by Discard = %v, want %v", dw.released, o.wantRelease)
				}
				if dw.discardCalls != 1 {
					t.Errorf("Discard calls = %d, want exactly 1 (deferred on every exit)", dw.discardCalls)
				}
			})
		}
	}
}
