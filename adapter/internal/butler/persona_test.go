package butler

import (
	"strings"
	"testing"
)

func TestDeviceSystemPrompt(t *testing.T) {
	withCwd := DeviceSystemPrompt("/Users/me/proj", "")
	if !strings.Contains(withCwd, "/Users/me/proj") {
		t.Errorf("expected cwd hint in prompt, got:\n%s", withCwd)
	}
	if !strings.Contains(withCwd, "PTT") || !strings.Contains(withCwd, "可朗读") {
		t.Errorf("expected device form-factor constraints in prompt, got:\n%s", withCwd)
	}

	// Empty / whitespace cwd must omit the cwd line entirely.
	for _, cwd := range []string{"", "   "} {
		if strings.Contains(DeviceSystemPrompt(cwd, ""), "当前工作目录") {
			t.Errorf("cwd=%q must omit the cwd line", cwd)
		}
	}

	// Non-empty deviceID must inject device-control section.
	withDevice := DeviceSystemPrompt("", "dev-abc-123")
	if !strings.Contains(withDevice, "dev-abc-123") {
		t.Errorf("expected deviceID in prompt, got:\n%s", withDevice)
	}
	if !strings.Contains(withDevice, "set-volume") {
		t.Errorf("expected set-volume CLI hint in prompt, got:\n%s", withDevice)
	}

	// Empty deviceID must omit device-control section.
	noDevice := DeviceSystemPrompt("/some/cwd", "")
	if strings.Contains(noDevice, "set-volume") {
		t.Errorf("empty deviceID must omit set-volume hint, got:\n%s", noDevice)
	}
}
