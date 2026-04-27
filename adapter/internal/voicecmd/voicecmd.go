// Package voicecmd matches short spoken phrases to OpenClaw slash commands.
// Interception happens between ASR and LLM so that /stop can cancel before
// the model runs.
package voicecmd

import "strings"

// Result holds a matched voice command.
type Result struct {
	Command string // e.g. "/stop", "/new", "/status"
}

var table = []struct {
	phrase  string
	command string
}{
	// stop / cancel
	{"停止", "/stop"},
	{"stop", "/stop"},
	{"取消", "/stop"},
	{"cancel", "/stop"},
	// new session
	{"新对话", "/new"},
	{"重新开始", "/new"},
	{"new", "/new"},
	// status
	{"状态", "/status"},
	{"status", "/status"},
}

// Match returns a Result if transcript is a voice command, nil otherwise.
func Match(transcript string) *Result {
	s := strings.TrimSpace(transcript)
	// strip trailing punctuation (ASCII + CJK fullwidth)
	s = strings.TrimRight(s, " .,!?;:")
	for _, suffix := range []string{"。", "！", "，", "？"} {
		s = strings.TrimSuffix(s, suffix)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, e := range table {
		if strings.EqualFold(s, e.phrase) {
			return &Result{Command: e.command}
		}
	}
	return nil
}
