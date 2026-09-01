package connector

import "testing"

// ModeFromStrings behaviour lock (OGA-892 / OGA-891 spec R7.2).
//
// This function REPLACES a kit-side implementation (oga-kit-sj24k's
// ontosync.ingressModeFor), so the point of this table is not that the mapping is
// desirable in the abstract — it is that the mapping is UNCHANGED. Moving the
// collapse into the SDK must be behaviour-preserving, or the kit that adopts it
// changes what its declared modes mean, which is exactly the drift the move
// exists to prevent.
func TestModeFromStrings(t *testing.T) {
	cases := []struct {
		name  string
		modes []string
		want  IngressMode
	}{
		// The documented default: an empty Mode is poll, so an empty list must be
		// too. A kit manifest may legitimately omit `modes`.
		{"nil", nil, ModePoll},
		{"empty slice", []string{}, ModePoll},

		{"poll only", []string{"poll"}, ModePoll},
		{"webhook only", []string{"webhook"}, ModeWebhook},
		{"both entries", []string{"webhook", "poll"}, ModeBoth},
		{"both entries reversed", []string{"poll", "webhook"}, ModeBoth},
		{"literal both", []string{"both"}, ModeBoth},
		{"both plus poll", []string{"both", "poll"}, ModeBoth},

		// Case and whitespace tolerance: these come from hand-written YAML.
		{"mixed case", []string{"WebHook"}, ModeWebhook},
		{"padded", []string{"  webhook  ", "\tpoll"}, ModeBoth},

		// An unrecognized entry is IGNORED, never promoted into a served mode.
		// Defense-in-depth: the manifest validator already rejects an unknown mode
		// at install, so this should not reach a running sidecar.
		{"unknown ignored alongside a known one", []string{"stream", "webhook"}, ModeWebhook},

		// ⚠️ The consequence worth pinning: a list of ONLY unrecognized entries is
		// indistinguishable from an empty list, so it yields poll. A connector that
		// must not silently fall back to polling has to cross-check its own config
		// and refuse to start — which is what the sj24k connector does.
		{"only unknown falls back to poll", []string{"stream", "grpc"}, ModePoll},
		{"empty strings only", []string{"", "   "}, ModePoll},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModeFromStrings(tc.modes); got != tc.want {
				t.Errorf("ModeFromStrings(%q) = %q, want %q", tc.modes, got, tc.want)
			}
		})
	}
}

// TestModeFromStrings_ResultIsAlwaysValid — whatever it returns must be a mode the
// server accepts, so a manifest can never produce a binding that ListenAndServe
// then rejects.
func TestModeFromStrings_ResultIsAlwaysValid(t *testing.T) {
	for _, modes := range [][]string{
		nil, {}, {"poll"}, {"webhook"}, {"both"},
		{"webhook", "poll"}, {"nonsense"}, {"", "  ", "nope"},
	} {
		if m := ModeFromStrings(modes); !m.valid() {
			t.Errorf("ModeFromStrings(%q) = %q, which is not a valid IngressMode", modes, m)
		}
	}
}

// TestModeFromStrings_AgreesWithPollAndWebhookEnabled — the resolved mode has to
// drive the two predicates the server actually branches on, so this asserts the
// end-to-end effect rather than just the enum value.
func TestModeFromStrings_AgreesWithPollAndWebhookEnabled(t *testing.T) {
	cases := []struct {
		modes       []string
		wantPoll    bool
		wantWebhook bool
	}{
		{nil, true, false},
		{[]string{"poll"}, true, false},
		{[]string{"webhook"}, false, true},
		{[]string{"webhook", "poll"}, true, true},
		{[]string{"both"}, true, true},
	}
	for _, tc := range cases {
		m := ModeFromStrings(tc.modes)
		if got := m.pollEnabled(); got != tc.wantPoll {
			t.Errorf("modes %q → %q: pollEnabled = %v, want %v", tc.modes, m, got, tc.wantPoll)
		}
		if got := m.webhookEnabled(); got != tc.wantWebhook {
			t.Errorf("modes %q → %q: webhookEnabled = %v, want %v", tc.modes, m, got, tc.wantWebhook)
		}
	}
}
