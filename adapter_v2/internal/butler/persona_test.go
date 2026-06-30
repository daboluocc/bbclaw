package butler

import (
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/projectstore"
)

// TestDeviceSystemPromptDeviceControl asserts the device-control section is always
// injected (v2's persona is static, so it can't gate on a live device id — it
// teaches the CLI, which resolves the current device itself).
func TestDeviceSystemPromptDeviceControl(t *testing.T) {
	p := DeviceSystemPrompt("/Users/me/proj", "", nil)

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
	// No projects → no project section.
	if strings.Contains(p, "用户的项目") {
		t.Error("empty project list should render no project section")
	}
}

// TestDeviceSystemPromptVoiceCommands asserts the ADR-042 command list is taught
// to the butler so it can tell the user which spoken shortcuts exist.
func TestDeviceSystemPromptVoiceCommands(t *testing.T) {
	p := DeviceSystemPrompt("/ws", "", nil)
	for _, want := range []string{
		"## 便捷口令",
		"停止 / 取消",
		"新对话 / 清空",
		"提醒我",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("DeviceSystemPrompt missing voice-command hint %q", want)
		}
	}
}

// TestDeviceSystemPromptProjects asserts a registered project is rendered with its
// purpose + directory + CLI status (ADR-036).
func TestDeviceSystemPromptProjects(t *testing.T) {
	projs := []projectstore.Project{
		{Name: "buildhub", Path: "/Users/me/buildhub", Summary: "内部构建平台", CLIBin: "definitely-not-installed-xyz"},
	}
	p := DeviceSystemPrompt("/ws", "", projs)
	for _, want := range []string{
		"## 用户的项目",
		"buildhub",
		"内部构建平台",
		"/Users/me/buildhub",
		"未配置", // the fake CLI is not on PATH
	} {
		if !strings.Contains(p, want) {
			t.Errorf("project section missing %q", want)
		}
	}
}
