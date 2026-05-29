package butler

import (
	"strings"
	"testing"
)

func TestDeviceSystemPrompt(t *testing.T) {
	withCwd := DeviceSystemPrompt("/Users/me/proj")
	if !strings.Contains(withCwd, "/Users/me/proj") {
		t.Errorf("expected cwd hint in prompt, got:\n%s", withCwd)
	}
	if !strings.Contains(withCwd, "PTT") || !strings.Contains(withCwd, "可朗读") {
		t.Errorf("expected device form-factor constraints in prompt, got:\n%s", withCwd)
	}

	// Empty / whitespace cwd must omit the cwd line entirely.
	for _, cwd := range []string{"", "   "} {
		if strings.Contains(DeviceSystemPrompt(cwd), "当前工作目录") {
			t.Errorf("cwd=%q must omit the cwd line", cwd)
		}
	}
}
