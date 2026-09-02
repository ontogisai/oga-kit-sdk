package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// PayloadValidator (OGA-892 follow-up).
//
// The gap it closes: in async mode a malformed body was answered 202, because the
// server ACKs before the handler runs — so from the caller's side a bad request was
// indistinguishable from a delivery that worked. In sync mode it was answered 500,
// because a handler cannot distinguish "your payload is bad" from "my downstream
// broke". Both are now 400 when the connector implements the interface.
//
// The properties that matter, and are easy to lose:
//
//   - the validator runs BEFORE the queue, so a rejected delivery is never enqueued
//     and the handler never sees it
//   - it runs in BOTH modes, so the two contracts agree
//   - a connector that does NOT implement it is completely unaffected
//   - it runs AFTER the connectPending gate, so a boot-window delivery is still a
//     retryable 503 rather than a 400 the caller would never retry

// payloadValidatingConnector is a fakeConnector that also validates payloads, recording
// whether the handler was reached.
type payloadValidatingConnector struct {
	*fakeConnector
	validateErr  error
	validateSeen atomic.Int32
	handlerSeen  atomic.Int32
}

func (v *payloadValidatingConnector) ValidateWebhookPayload(_ context.Context, _ Binding, _ []byte) error {
	v.validateSeen.Add(1)
	return v.validateErr
}

// newValidatingConnector wires a connector whose handler records that it ran.
func newPayloadValidatingConnector(validateErr error) *payloadValidatingConnector {
	v := &payloadValidatingConnector{validateErr: validateErr}
	v.fakeConnector = &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
	}
	v.webhookFn = func(ctx context.Context, _ Binding, _ []byte, em *Emitter) error {
		v.handlerSeen.Add(1)
		return em.Entities.WriteVertex(ctx, transfer.Vertex{ID: "e1", EntityType: "T"})
	}
	return v
}

func postWebhook(t *testing.T, h http.Handler, body string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestPayloadValidator_RejectsWith400_Async is the headline fix.
func TestPayloadValidator_RejectsWith400_Async(t *testing.T) {
	vc := newPayloadValidatingConnector(errors.New("presigned_url is required"))
	s := newAsyncTestServer(t, vc, func(context.Context, Binding) (transfer.Writer, error) {
		return &fakeWriter{}, nil
	}, 4)

	resp := postWebhook(t, s.mux(), `{"version":"42"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (async must not answer 202 for a malformed body)", resp.StatusCode)
	}
	if vc.validateSeen.Load() != 1 {
		t.Errorf("validator ran %d times, want 1", vc.validateSeen.Load())
	}
	// The delivery must never have been queued: the handler running would mean the
	// 400 was cosmetic and the bad payload was processed anyway.
	if got := vc.handlerSeen.Load(); got != 0 {
		t.Errorf("handler ran %d times for a rejected payload; it must not be enqueued", got)
	}
}

// TestPayloadValidator_RejectsWith400_Sync — the sync path improves too: 400 rather
// than the 500 a handler error maps to.
func TestPayloadValidator_RejectsWith400_Sync(t *testing.T) {
	vc := newPayloadValidatingConnector(errors.New("presigned_url is required"))
	s := newTestServer(vc, func(context.Context, Binding) (transfer.Writer, error) {
		return &fakeWriter{}, nil
	})

	resp := postWebhook(t, s.mux(), `{"version":"42"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := vc.handlerSeen.Load(); got != 0 {
		t.Errorf("handler ran %d times for a rejected payload", got)
	}
}

// TestPayloadValidator_ErrorMessageReachesTheCaller — a 400 with no detail leaves
// the upstream team guessing, which is most of what made the silent 202 expensive.
func TestPayloadValidator_ErrorMessageReachesTheCaller(t *testing.T) {
	vc := newPayloadValidatingConnector(errors.New("presigned_url is required"))
	s := newAsyncTestServer(t, vc, func(context.Context, Binding) (transfer.Writer, error) {
		return &fakeWriter{}, nil
	}, 4)

	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/webhook/wo", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if body := string(buf[:n]); !strings.Contains(body, "presigned_url is required") {
		t.Errorf("response body %q does not carry the validator's reason", body)
	}
}

// TestPayloadValidator_AcceptedPayloadProceeds — a passing validator must be
// transparent, in both modes.
func TestPayloadValidator_AcceptedPayloadProceeds(t *testing.T) {
	t.Run("async", func(t *testing.T) {
		vc := newPayloadValidatingConnector(nil)
		done := make(chan struct{})
		vc.webhookFn = func(ctx context.Context, _ Binding, _ []byte, em *Emitter) error {
			vc.handlerSeen.Add(1)
			defer close(done)
			return em.Entities.WriteVertex(ctx, transfer.Vertex{ID: "e1", EntityType: "T"})
		}
		s := newAsyncTestServer(t, vc, func(context.Context, Binding) (transfer.Writer, error) {
			return &fakeWriter{}, nil
		}, 4)
		resp := postWebhook(t, s.mux(), `{"presigned_url":"https://x/y"}`)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", resp.StatusCode)
		}
		<-done
		if vc.handlerSeen.Load() != 1 {
			t.Error("handler did not run for an accepted payload")
		}
	})

	t.Run("sync", func(t *testing.T) {
		vc := newPayloadValidatingConnector(nil)
		s := newTestServer(vc, func(context.Context, Binding) (transfer.Writer, error) {
			return &fakeWriter{}, nil
		})
		resp := postWebhook(t, s.mux(), `{"presigned_url":"https://x/y"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if vc.handlerSeen.Load() != 1 {
			t.Error("handler did not run for an accepted payload")
		}
	})
}

// TestPayloadValidator_NotImplementedIsUnaffected — property 12's spirit: a
// connector that does not implement the interface must behave exactly as before, so
// this is additive.
func TestPayloadValidator_NotImplementedIsUnaffected(t *testing.T) {
	ran := make(chan struct{})
	fc := &fakeConnector{
		bindings: []Binding{{ID: "wo", Mode: ModeWebhook}},
		webhookFn: func(ctx context.Context, _ Binding, _ []byte, em *Emitter) error {
			defer close(ran)
			return em.Entities.WriteVertex(ctx, transfer.Vertex{ID: "e1", EntityType: "T"})
		},
	}
	s := newAsyncTestServer(t, fc, func(context.Context, Binding) (transfer.Writer, error) {
		return &fakeWriter{}, nil
	}, 4)

	// Deliberately the body that WOULD be rejected by a validator: without one, the
	// server must still accept it and let the handler decide.
	resp := postWebhook(t, s.mux(), `not even json`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a connector with no validator", resp.StatusCode)
	}
	<-ran
}

// TestPayloadValidator_RunsAfterTheConnectPendingGate is the ordering that is easy
// to get backwards. During the boot window the honest answer is a RETRYABLE 503; a
// 400 would tell the caller to change a request that was fine, and it will not
// retry. So the gate must win.
func TestPayloadValidator_RunsAfterTheConnectPendingGate(t *testing.T) {
	vc := newPayloadValidatingConnector(errors.New("presigned_url is required"))
	s := newAsyncTestServer(t, vc, func(context.Context, Binding) (transfer.Writer, error) {
		return &fakeWriter{}, nil
	}, 4)
	s.connectPending.Store(true)

	resp := postWebhook(t, s.mux(), `{"version":"42"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — the connect gate must outrank payload validation",
			resp.StatusCode)
	}
	if got := vc.validateSeen.Load(); got != 0 {
		t.Errorf("validator ran %d times during the boot window; the gate should have "+
			"short-circuited before it", got)
	}
}

// TestPayloadValidator_DoesNotAffectTheValidationHandshake — the two interfaces are
// easy to conflate. The GET handshake proves endpoint ownership and carries no
// payload, so a payload validator must have no say in it.
func TestPayloadValidator_DoesNotAffectTheValidationHandshake(t *testing.T) {
	vc := newPayloadValidatingConnector(errors.New("presigned_url is required"))
	s := newAsyncTestServer(t, vc, func(context.Context, Binding) (transfer.Writer, error) {
		return &fakeWriter{}, nil
	}, 4)

	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/webhook/wo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("validation handshake = %d, want 200; a payload validator must not gate it",
			resp.StatusCode)
	}
	if got := vc.validateSeen.Load(); got != 0 {
		t.Errorf("payload validator ran %d times on the GET handshake", got)
	}
}
