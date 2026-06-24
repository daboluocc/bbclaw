// Package curdevice records the most-recently-active BBClaw device id to a small
// file in the data dir, so a SEPARATE short-lived CLI process (the `device`
// subcommand) can target "the device the user is currently talking through"
// without the caller passing an id.
//
// Why this exists: adapter_v2's butler persona is STATIC (built once at boot via
// --append-system-prompt, no per-turn device id like v1 injected). So "self
// volume control" can't bake the device id into the prompt — the id must be
// discoverable out-of-band. The running adapter writes it whenever a device
// connects (LAN devicews hello) or sends a cloud-relay request; the `device
// set-volume`/`set-miyu` CLI reads it as the default --device.
//
// The file is a plain trimmed device-id string under DataDir()/current-device.
// A missing/unreadable file just yields "" (the CLI then needs an explicit
// --device, or reports that no device is connected).
package curdevice

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/settingsstore"
)

// fileName is the data-dir file holding the current device id.
const fileName = "current-device"

// path returns the data-dir path of the current-device marker.
func path() string {
	return filepath.Join(settingsstore.DataDir(), fileName)
}

// Record persists id as the current device. Empty/whitespace ids are ignored —
// callers pass only REAL device ids (hello.Dev / envelope deviceId), never the
// synthetic "dev-anon-*" LAN placeholders, which are not cloud device ids the
// config API understands. An unchanged value is a no-op so per-turn calls from
// the cloud relay don't churn the disk. Best-effort: the error is returned for
// logging, but a stale marker is non-fatal so callers generally ignore it.
func Record(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || Get() == id {
		return nil
	}
	p := path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(id+"\n"), 0o600)
}

// Get returns the recorded current device id, or "" if none is set / unreadable.
func Get() string {
	raw, err := os.ReadFile(path())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
