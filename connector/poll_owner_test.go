package connector

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// The platform-owned poll schedule (OGA-1372): with OGA_CONNECTOR_POLL_OWNER=platform
// the connector runs no poll loop, and a drain the schedule starts reports itself as
// one, which is how the platform tells a connector that honours its schedule from an
// older one.

func TestSyncNow_ScheduleTriggerIsReported(t *testing.T) {
	fc := &fakeConnector{
		bindings: []Binding{{ID: "assets", Mode: ModePoll}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			return &SyncResult{}, nil
		},
	}
	factory, _ := commitFactory()
	s := newTestServer(fc, factory)
	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	url := srv.URL + SyncPath("assets")

	code, st := syncRequest(t, http.MethodPost, url+"?"+SyncTriggerParam+"="+string(SyncTriggerSchedule))
	if code != http.StatusAccepted || st.Trigger != SyncTriggerSchedule {
		t.Fatalf("scheduled POST = %d %+v, want 202 with trigger schedule", code, st)
	}
	if st = waitFinished(t, url); st.Trigger != SyncTriggerSchedule || st.Outcome != SyncSucceeded {
		t.Errorf("GET after a scheduled drain = %+v, want a succeeded schedule drain", st)
	}

	// Anything but the schedule value is an operator's sync now.
	for _, q := range []string{"", "?" + SyncTriggerParam + "=poll", "?" + SyncTriggerParam + "=Schedule"} {
		code, st = syncRequest(t, http.MethodPost, url+q)
		if code != http.StatusAccepted || st.Trigger != SyncTriggerManual {
			t.Errorf("POST%s = %d %+v, want 202 with trigger manual", q, code, st)
		}
		waitFinished(t, url)
	}
}

func TestPlatformOwnsPolling_ReadsTheEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", false}, {"sidecar", false}, {"Platform", false}, {PollOwnerPlatform, true}} {
		t.Setenv(EnvPollOwner, tc.value)
		if got := PlatformOwnsPolling(); got != tc.want {
			t.Errorf("%s=%q: PlatformOwnsPolling() = %v, want %v", EnvPollOwner, tc.value, got, tc.want)
		}
	}
}

// With the platform owning the schedule, no tick drains the binding, not even the
// first drain after Connect; a scheduled POST still does.
func TestListenAndServe_PlatformOwnedRunsNoPollLoop(t *testing.T) {
	t.Setenv(EnvPollOwner, PollOwnerPlatform)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	fc := &fakeConnector{
		bindings: []Binding{{ID: "pull", Mode: ModePoll}},
		health:   map[string]Health{"pull": {OK: true}},
		syncFn: func(context.Context, Binding, string, *Emitter) (*SyncResult, error) {
			calls.Add(1)
			return &SyncResult{}, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	factory, _ := commitFactory()
	cfg := &Config{
		Port:          port,
		WriterFactory: factory,
		PollInterval:  time.Millisecond, // would tick hundreds of times below
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, cfg, fc) }()

	url := "http://127.0.0.1:" + port + SyncPath("pull")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _ := tryRequest(http.MethodGet, url); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never answered its sync status")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("Sync called %d times with the platform owning the schedule, want 0", n)
	}
	if code, st := tryRequest(http.MethodGet, url); code != http.StatusOK || st.Outcome != "" {
		t.Errorf("status before any scheduled drain = %d %+v, want no drain", code, st)
	}

	// The connector answers 503 until Connect has finished; retry until it is ready.
	for {
		code, st := tryRequest(http.MethodPost, url+"?"+SyncTriggerParam+"="+string(SyncTriggerSchedule))
		if code == http.StatusAccepted {
			if st.Trigger != SyncTriggerSchedule {
				t.Errorf("scheduled POST trigger = %q, want schedule", st.Trigger)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduled POST never accepted (last %d)", code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for calls.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("Sync called %d times after the scheduled POST, want 1", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("Sync called %d times, want exactly the scheduled drain", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe = %v, want nil", err)
	}
}
