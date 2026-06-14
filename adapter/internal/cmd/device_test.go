package cmd

import (
	"encoding/json"
	"testing"
)

func TestParseOnOff(t *testing.T) {
	on := []string{"on", "On", "ON", "true", "1", "enable", "enabled", "yes", " on "}
	for _, s := range on {
		v, err := parseOnOff(s)
		if err != nil {
			t.Errorf("parseOnOff(%q) returned error: %v", s, err)
		}
		if !v {
			t.Errorf("parseOnOff(%q) = false, want true", s)
		}
	}

	off := []string{"off", "Off", "OFF", "false", "0", "disable", "disabled", "no", " off "}
	for _, s := range off {
		v, err := parseOnOff(s)
		if err != nil {
			t.Errorf("parseOnOff(%q) returned error: %v", s, err)
		}
		if v {
			t.Errorf("parseOnOff(%q) = true, want false", s)
		}
	}

	for _, s := range []string{"", "maybe", "2", "onoff"} {
		if _, err := parseOnOff(s); err == nil {
			t.Errorf("parseOnOff(%q) expected error, got nil", s)
		}
	}
}

func TestDeviceCmdRegistersSubcommands(t *testing.T) {
	dev := NewDeviceCmd()
	want := map[string]bool{"set-volume": false, "set-miyu": false}
	for _, c := range dev.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("device command missing subcommand %q", name)
		}
	}
}

// TestSetConfigRequestOmitsUnsetFields guards the partial-update contract: a
// set-miyu request must NOT serialize volumePct (which would mute the device),
// and vice-versa.
func TestSetConfigRequestOmitsUnsetFields(t *testing.T) {
	enabled := true
	miyuBody, _ := json.Marshal(setConfigRequest{MiyuEnabled: &enabled})
	if got := string(miyuBody); got != `{"miyuEnabled":true}` {
		t.Errorf("miyu-only body = %s, want only miyuEnabled", got)
	}

	pct := 50
	volBody, _ := json.Marshal(setConfigRequest{VolumePct: &pct})
	if got := string(volBody); got != `{"volumePct":50}` {
		t.Errorf("volume-only body = %s, want only volumePct", got)
	}

	// A zero volume must still serialize when explicitly set (mute is valid).
	zero := 0
	zeroBody, _ := json.Marshal(setConfigRequest{VolumePct: &zero})
	if got := string(zeroBody); got != `{"volumePct":0}` {
		t.Errorf("zero-volume body = %s, want volumePct:0", got)
	}
}
