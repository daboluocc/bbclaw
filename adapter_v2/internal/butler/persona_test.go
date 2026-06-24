package butler

import (
	"strings"
	"testing"
)

// TestDeviceSystemPromptDeviceControl asserts the device-control section is always
// injected (v2's persona is static, so it can't gate on a live device id — it
// teaches the CLI, which resolves the current device itself).
func TestDeviceSystemPromptDeviceControl(t *testing.T) {
	p := DeviceSystemPrompt("/Users/me/proj", "")

	for _, want := range []string{
		"## 设备控制",
		"device set-volume <0-100>",
		"device set-miyu <on|off>",
		"$BBCLAW_ADAPTER_V2_BIN",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("DeviceSystemPrompt missing %q", want)
		}
	}
	if !strings.Contains(p, "/Users/me/proj") {
		t.Error("DeviceSystemPrompt dropped the cwd hint")
	}
}
