package butler

import "strings"

// DeviceSystemPrompt builds the BBClaw "butler" system prompt injected into
// every turn via StartOpts.SystemPrompt (claudecode → --append-system-prompt,
// ADR-018 §3). It encodes the product persona and the hard constraints of the
// device form factor: a tiny screen, voice output, push-to-talk input. The
// goal is to make the backend behave like it is talking to a walkie-talkie,
// not a full terminal — short, speakable answers instead of walls of code.
//
// cwd is the active project directory; when set it is surfaced as an explicit
// hint (the CLI also runs in it, but stating it removes ambiguity).
//
// deviceID is the current device's ID injected per-turn. When non-empty, a
// device-control section is appended so the butler knows how to call the
// bbclaw-adapter device CLI to adjust volume. Empty deviceID = skip that section.
//
// This is the static baseline. ADR-018 P1 will append a user-needs summary and
// project memory here once the memory store exists.
func DeviceSystemPrompt(cwd, deviceID string) string {
	var b strings.Builder
	b.WriteString("你正通过 BBClaw 与用户对话——一台对讲机式的硬件语音外设" +
		"(1.47 寸小屏、PTT 按键、语音播报),作为 AI 编码助手 CLI 的远程终端。" +
		"用户通常不在电脑前,只能靠语音和小屏与你交互。\n\n")
	b.WriteString("请遵守:\n")
	b.WriteString("- 回答简短、可朗读:先用 1-3 句话给出结论;避免长代码块、表格、大段列表" +
		"(小屏放不下,也无法朗读)。\n")
	b.WriteString("- 必须展示代码或长输出时,先一句话口述要点,再给最小必要片段。\n")
	b.WriteString("- 用用户的语言回答。\n")
	b.WriteString("- 工具调用(读写文件、执行命令)会真实作用于本地项目,请谨慎、按需。\n")
	if c := strings.TrimSpace(cwd); c != "" {
		b.WriteString("- 当前工作目录:" + c + "\n")
	}
	if id := strings.TrimSpace(deviceID); id != "" {
		b.WriteString("\n## 设备控制\n")
		b.WriteString("当前设备 ID:`" + id + "`\n")
		b.WriteString("如需调节本设备音量,用 Bash 工具执行:\n")
		b.WriteString("  bbclaw-adapter device set-volume <0-100> --device " + id + "\n")
		b.WriteString("成功后用一句话朗读结果(例如「已把音量调到 50%,马上生效」)。\n")
		b.WriteString("如果设备 ID 未知,不要尝试音量调节。\n")
	}
	return b.String()
}
