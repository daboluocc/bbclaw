package main

import (
	"encoding/json"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/curdevice"
)

func TestParseOnOff(t *testing.T) {
	on := []string{"on", "On", "ON", "true", "1", "enable", "enabled", "yes", " on "}
	for _, s := range on {
		v, err := parseOnOff(s)
		if err != nil || !v {
			t.Errorf("parseOnOff(%q) = (%v,%v), want (true,nil)", s, v, err)
		}
	}
	off := []string{"off", "Off", "OFF", "false", "0", "disable", "disabled", "no", " off "}
	for _, s := range off {
		v, err := parseOnOff(s)
		if err != nil || v {
			t.Errorf("parseOnOff(%q) = (%v,%v), want (false,nil)", s, v, err)
		}
	}
	for _, s := range []string{"", "maybe", "2", "onoff"} {
		if _, err := parseOnOff(s); err == nil {
			t.Errorf("parseOnOff(%q) expected error, got nil", s)
		}
	}
}

// TestSetConfigRequestOmitsUnsetFields guards the partial-update contract: a
// set-miyu request must NOT serialize volumePct (which would mute the device),
// and vice-versa. Mirrors v1's guard.
func TestSetConfigRequestOmitsUnsetFields(t *testing.T) {
	enabled := true
	if got, _ := json.Marshal(setConfigRequest{MiyuEnabled: &enabled}); string(got) != `{"miyuEnabled":true}` {
		t.Errorf("miyu-only body = %s, want only miyuEnabled", got)
	}
	pct := 0
	if got, _ := json.Marshal(setConfigRequest{VolumePct: &pct}); string(got) != `{"volumePct":0}` {
		t.Errorf("volume-only body = %s, want only volumePct (incl. 0=mute)", got)
	}
}

func TestCloudHTTPBase(t *testing.T) {
	cases := []struct {
		ws   string
		want string
	}{
		{"wss://bbclaw.daboluo.cc/ws", "https://bbclaw.daboluo.cc"},
		{"ws://192.168.1.9:18080/ws", "http://192.168.1.9:18080"},
		{"https://example.com/ws?token=x", "https://example.com"},
		{"", "https://bbclaw.daboluo.cc"}, // default when unset
	}
	for _, c := range cases {
		t.Setenv("CLOUD_WS_URL", c.ws)
		got, err := cloudHTTPBase()
		if err != nil {
			t.Errorf("cloudHTTPBase(%q) error: %v", c.ws, err)
			continue
		}
		if got != c.want {
			t.Errorf("cloudHTTPBase(%q) = %q, want %q", c.ws, got, c.want)
		}
	}
}

func TestResolveDeviceID(t *testing.T) {
	t.Setenv("BBCLAW_ADAPTER_V2_DATA_DIR", t.TempDir())

	// Explicit flag always wins.
	if got := resolveDeviceID("dev-flag"); got != "dev-flag" {
		t.Errorf("resolveDeviceID(flag) = %q, want dev-flag", got)
	}
	// No flag, no recorded device → empty (caller errors out).
	if got := resolveDeviceID(""); got != "" {
		t.Errorf("resolveDeviceID(empty, none recorded) = %q, want \"\"", got)
	}
	// No flag → falls back to the recorded current device.
	if err := curdevice.Record("dev-current"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := resolveDeviceID(""); got != "dev-current" {
		t.Errorf("resolveDeviceID(empty) = %q, want dev-current", got)
	}
}
