package cloudrelay

import "testing"

func TestNotifyEmitsSessionNotification(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}
	var got Envelope
	r.setSend(func(e Envelope) error { got = e; return nil })

	if err := r.Notify("dev-1", "提醒：检查烧录日志", "提醒，检查烧录日志", "ls-7"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got.Type != "event" || got.Kind != "session.notification" {
		t.Errorf("envelope type/kind = %q/%q, want event/session.notification", got.Type, got.Kind)
	}
	if got.DeviceID != "dev-1" || got.HomeSiteID != "site" {
		t.Errorf("deviceId/homeSite = %q/%q", got.DeviceID, got.HomeSiteID)
	}
	if got.Payload["preview"] != "提醒：检查烧录日志" {
		t.Errorf("preview = %v", got.Payload["preview"])
	}
	if got.Payload["sessionId"] != "ls-7" {
		t.Errorf("sessionId = %v", got.Payload["sessionId"])
	}
	// TTS opt-in: speak flag + ttsText present when ttsText is supplied.
	if got.Payload["speak"] != true {
		t.Errorf("speak = %v, want true", got.Payload["speak"])
	}
	if got.Payload["ttsText"] != "提醒，检查烧录日志" {
		t.Errorf("ttsText = %v", got.Payload["ttsText"])
	}
}

func TestNotifyWithoutTTSOmitsSpeak(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}
	var got Envelope
	r.setSend(func(e Envelope) error { got = e; return nil })
	if err := r.Notify("dev-1", "提醒：看日志", "", "ls-1"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if _, ok := got.Payload["speak"]; ok {
		t.Error("empty ttsText should not set speak (old-firmware = toast only)")
	}
	if _, ok := got.Payload["ttsText"]; ok {
		t.Error("empty ttsText should not be present in payload")
	}
}

func TestNotifyBuffersWhenDisconnected(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}, log: func(string, ...any) {}}
	// send is nil (never connected) → Notify buffers to the outbox and returns nil
	// (the reminder is delivered late, not dropped — Task #5).
	if err := r.Notify("dev-1", "提醒：看日志", "提醒，看日志", "s"); err != nil {
		t.Fatalf("Notify while disconnected should buffer, got err %v", err)
	}
	// Reconnect: flushOutbox re-sends the buffered notification.
	var got []Envelope
	r.flushOutbox(func(e Envelope) error { got = append(got, e); return nil })
	if len(got) != 1 {
		t.Fatalf("flushOutbox sent %d, want 1", len(got))
	}
	if got[0].DeviceID != "dev-1" || got[0].Payload["ttsText"] != "提醒，看日志" {
		t.Errorf("flushed envelope wrong: %+v", got[0])
	}
	// Outbox is now empty — a second flush sends nothing.
	got = nil
	r.flushOutbox(func(e Envelope) error { got = append(got, e); return nil })
	if len(got) != 0 {
		t.Errorf("second flush sent %d, want 0 (outbox should be drained)", len(got))
	}
}

func TestFlushOutboxRebuffersOnFailure(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}, log: func(string, ...any) {}}
	r.Notify("dev-1", "a", "", "s")
	r.Notify("dev-2", "b", "", "s")
	// First flush fails on the 2nd send → the 2nd must stay buffered.
	n := 0
	r.flushOutbox(func(Envelope) error {
		n++
		if n == 2 {
			return errContext
		}
		return nil
	})
	// Retry flush should deliver exactly the one that didn't get through.
	var got []Envelope
	r.flushOutbox(func(e Envelope) error { got = append(got, e); return nil })
	if len(got) != 1 || got[0].DeviceID != "dev-2" {
		t.Fatalf("re-buffered/retry wrong: %+v", got)
	}
}

var errContext = errText("send failed")

type errText string

func (e errText) Error() string { return string(e) }

func TestNotifyRequiresDeviceID(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}
	r.setSend(func(Envelope) error { return nil })
	if err := r.Notify("", "x", "", "s"); err == nil {
		t.Error("Notify with empty deviceId = nil err, want error")
	}
}
