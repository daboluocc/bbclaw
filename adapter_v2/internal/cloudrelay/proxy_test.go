package cloudrelay

import (
	"strings"
	"testing"
)

// The device settings page fires these kinds; each must get a reply (so the
// device's HTTP call resolves instead of timing out to ESP_ERR_HTTP_EAGAIN) with
// the field names the cloud extracts.
func TestProxyAnswersSettingsKinds(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}

	cases := []struct {
		kind      string
		wantField string // a field the cloud/firmware reads from the reply
	}{
		{"agent.drivers", "drivers"},
		{"agent.messages", "messages"},
		{"agent.sessions", "sessions"},
		{"agent.sessions.list.logical", "sessions"}, // the kind the firmware actually fires
		{"agent.cwd_pool", "pool"},
		{"agent.active_driver.set", "active_driver"},
		{"agent.menu", "rows"},
		{"agent.menu.action", "result"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			var got Envelope
			n := 0
			ok := r.handleAgentProxy(func(e Envelope) error { got = e; n++; return nil },
				Envelope{Kind: tc.kind, MessageID: "m1", DeviceID: "d1", Payload: map[string]any{"id": "drivers"}})
			if !ok {
				t.Fatalf("%s not handled (would hang the device)", tc.kind)
			}
			if n != 1 || got.Type != "reply" || got.MessageID != "m1" {
				t.Fatalf("want one reply echoing messageId, got type=%q n=%d id=%q", got.Type, n, got.MessageID)
			}
			if _, has := got.Payload[tc.wantField]; !has {
				t.Errorf("reply payload missing %q field: %v", tc.wantField, got.Payload)
			}
		})
	}

	// agent.drivers must advertise the single claude driver as active.
	var drv Envelope
	r.handleAgentProxy(func(e Envelope) error { drv = e; return nil },
		Envelope{Kind: "agent.drivers", MessageID: "m"})
	if drv.Payload["active_driver"] != proxyDriver {
		t.Errorf("active_driver = %v, want %q", drv.Payload["active_driver"], proxyDriver)
	}

	// An unknown kind is not our concern → false (caller no-ops).
	if r.handleAgentProxy(func(Envelope) error { return nil }, Envelope{Kind: "something.else"}) {
		t.Error("unknown kind should return false")
	}
}

func TestVoicePromptScopedToClaude(t *testing.T) {
	// claude CLI → voice persona appended.
	got := withVoicePrompt([]string{"/usr/local/bin/claude"})
	if len(got) != 3 || got[1] != "--append-system-prompt" || !strings.Contains(got[2], "简短") {
		t.Errorf("claude argv not augmented: %v", got)
	}
	// Non-claude CLI (e.g. the e2e mockcli) → untouched, so other CLIs/tests are safe.
	if base := withVoicePrompt([]string{"/tmp/mockcli"}); len(base) != 1 {
		t.Errorf("non-claude argv should be untouched, got %v", base)
	}
	// Explicit empty env disables it.
	t.Setenv("ADAPTER_V2_VOICE_SYSTEM_PROMPT", "")
	if off := withVoicePrompt([]string{"claude"}); len(off) != 1 {
		t.Errorf("empty env should disable the prompt, got %v", off)
	}
	// Custom env overrides the default.
	t.Setenv("ADAPTER_V2_VOICE_SYSTEM_PROMPT", "be terse")
	if cust := withVoicePrompt([]string{"claude"}); len(cust) != 3 || cust[2] != "be terse" {
		t.Errorf("custom env not applied, got %v", cust)
	}
}
