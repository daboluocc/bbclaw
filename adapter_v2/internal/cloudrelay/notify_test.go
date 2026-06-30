package cloudrelay

import "testing"

func TestNotifyEmitsSessionNotification(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}
	var got Envelope
	r.setSend(func(e Envelope) error { got = e; return nil })

	if err := r.Notify("dev-1", "提醒：检查烧录日志", "ls-7"); err != nil {
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
}

func TestNotifyErrorsWhenDisconnected(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}
	// send is nil (never connected) → Notify must surface an error, not panic.
	if err := r.Notify("dev-1", "x", "s"); err == nil {
		t.Error("Notify with no cloud link = nil err, want error")
	}
}

func TestNotifyRequiresDeviceID(t *testing.T) {
	r := &Relay{cfg: Config{HomeSiteID: "site"}}
	r.setSend(func(Envelope) error { return nil })
	if err := r.Notify("", "x", "s"); err == nil {
		t.Error("Notify with empty deviceId = nil err, want error")
	}
}
