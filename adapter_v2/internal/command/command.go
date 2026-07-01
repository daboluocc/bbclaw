// Package command is the adapter_v2 command router (ADR-042): it turns a short
// spoken/typed phrase into a structured Intent so quick commands ("停止", "状态",
// "新对话", "30 分钟后提醒我…") are handled WITHOUT going to the CLI/LLM.
//
// Interception happens between ASR and PTY injection (deviceapi.SubmitVoiceTurn):
// a matched command short-circuits the turn, so "停止" cancels instead of being
// typed at claude as a billable prompt. Parsing here is PURE (no I/O, no Bridge
// dependency) — execution lives in the caller, which owns Interrupt/speak/scheduler.
//
// Strategy (ADR-042 §2.4), short-circuit in order:
//  1. exact phrase table first — guarantees "停止" never reaches the LLM and is
//     never mistaken for a time expression.
//  2. lightweight time-expression rules for reminders (X 分钟后 / X 小时后 /
//     明天 HH:mm). Fuzzy natural-language time is P1 (Butler-assisted).
package command

import (
	"fmt"
	"regexp"
	"strings"
)

// Intent kinds. Stable strings — also used as cloud/device envelope kinds.
const (
	KindCancel         = "turn.cancel"
	KindNewSession     = "session.new"
	KindStatus         = "status.show"
	KindReminderCreate = "reminder.create"
	KindReminderList   = "reminder.list"
)

// Intent is the structured result of parsing one phrase. Args carries kind-
// specific fields, e.g. reminder.create: {"delay":"30m","prompt":"检查烧录日志"}
// or {"at":"2026-06-30T21:30","prompt":"…"}.
type Intent struct {
	Kind   string            `json:"kind"`
	Text   string            `json:"text"`   // original transcript
	Source string            `json:"source"` // "voice" / "http" / "cloud"
	Args   map[string]string `json:"args,omitempty"`
}

// phrase table — exact (case-insensitive) matches that bypass the LLM. Mirrors
// v1 internal/voicecmd, extended with reminder.list. Kept small on purpose: a
// false positive here silently eats a real user turn, so only unambiguous
// command words belong.
var phraseTable = []struct {
	phrase string
	kind   string
}{
	// stop / cancel
	{"停止", KindCancel},
	{"取消", KindCancel},
	{"停", KindCancel},
	{"stop", KindCancel},
	{"cancel", KindCancel},
	// new session
	{"新对话", KindNewSession},
	{"新会话", KindNewSession},
	{"重新开始", KindNewSession},
	{"清空", KindNewSession},
	{"new", KindNewSession},
	// status
	{"状态", KindStatus},
	{"现在怎样", KindStatus},
	{"什么情况", KindStatus},
	{"status", KindStatus},
	// reminder list
	{"看提醒", KindReminderList},
	{"有哪些提醒", KindReminderList},
	{"提醒列表", KindReminderList},
}

// Parse returns a structured Intent if transcript is a command, or nil if it is
// an ordinary turn that should go to the CLI. source tags the entry point
// ("voice"/"http"/"cloud") so one Router serves all callers (ADR-042 §2.4).
func Parse(transcript, source string) *Intent {
	s := normalize(transcript)
	if s == "" {
		return nil
	}
	low := strings.ToLower(s)
	for _, e := range phraseTable {
		if low == strings.ToLower(e.phrase) {
			return &Intent{Kind: e.kind, Text: transcript, Source: source}
		}
	}
	if in := parseReminder(s, transcript, source); in != nil {
		return in
	}
	return nil
}

// normalize trims surrounding whitespace and trailing punctuation (ASCII + CJK
// fullwidth), matching v1 voicecmd so "停止。" and "停止" both match.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, " .,!?;:")
	for _, suffix := range []string{"。", "！", "，", "？", "、", "；", "："} {
		s = strings.TrimSuffix(s, suffix)
	}
	return strings.TrimSpace(s)
}

// --- reminder time-expression parsing (ADR-042 §2.4 rule 2) -------------------
//
// P0 grammar, deliberately narrow:
//   <N> 分钟后  → Args{delay: "<N>m"}
//   <N> 小时后  → Args{delay: "<N>h"}
//   明天 HH:mm  → Args{at: "tomorrow HH:MM"}
// Whatever text remains after stripping the time + a leading "提醒我"/"提醒" verb
// becomes the prompt. A phrase with a time expression but no remaining prompt
// still parses (prompt defaults empty; the executor supplies a default).

var (
	reMinutes  = regexp.MustCompile(`(\d+)\s*分钟?后`)
	reHours    = regexp.MustCompile(`(\d+)\s*(?:个)?\s*小时后`)
	reTomorrow = regexp.MustCompile(`明天\s*(\d{1,2})[:：点](\d{1,2})?`)
)

func parseReminder(s, original, source string) *Intent {
	// A reminder must look like one: either a recognised time expression OR an
	// explicit "提醒" verb. Bare "明天去开会" without 提醒/时间点 is NOT a command.
	hasRemindVerb := strings.Contains(s, "提醒") || strings.Contains(strings.ToLower(s), "remind")

	if m := reMinutes.FindStringSubmatch(s); m != nil {
		return reminder(map[string]string{"delay": m[1] + "m"}, stripTime(s, m[0]), original, source)
	}
	if m := reHours.FindStringSubmatch(s); m != nil {
		return reminder(map[string]string{"delay": m[1] + "h"}, stripTime(s, m[0]), original, source)
	}
	if m := reTomorrow.FindStringSubmatch(s); m != nil {
		hh, mm := m[1], m[2]
		if mm == "" {
			mm = "00"
		}
		at := fmt.Sprintf("tomorrow %s:%s", pad2(hh), pad2(mm))
		return reminder(map[string]string{"at": at}, stripTime(s, m[0]), original, source)
	}
	// time verb present but no parseable time → still a reminder.create so the
	// caller can ask the user to restate; prompt = the remaining text.
	if hasRemindVerb {
		return reminder(map[string]string{}, s, original, source)
	}
	return nil
}

func reminder(args map[string]string, rest, original, source string) *Intent {
	prompt := cleanPrompt(rest)
	if prompt != "" {
		args["prompt"] = prompt
	}
	args["mode"] = inferMode(prompt)
	return &Intent{Kind: KindReminderCreate, Text: original, Source: source, Args: args}
}

// taskCues are explicit "do a task for me" verbs. Their presence flips a reminder
// from a plain alarm (ModeNotify) to a proactive task the adapter RUNS and reports
// (ModeTask, ADR-042 §3.3). Kept conservative — bare "看/查" alone stays an alarm;
// only clear delegation ("帮我…", "…并告诉我", "汇报") triggers a task, so a simple
// "提醒我看日志" isn't turned into an autonomous agent run by surprise.
var taskCues = []string{"帮我", "汇报", "报告", "告诉我", "统计", "分析", "总结", "查一下", "检查一下", "run ", "report"}

// inferMode returns "task" when the prompt asks the adapter to DO something and
// report back, else "notify" (a plain reminder). Fuzzy NLU is P1 (ADR-042 §2.4).
func inferMode(prompt string) string {
	low := strings.ToLower(prompt)
	for _, c := range taskCues {
		if strings.Contains(low, c) {
			return "task"
		}
	}
	return "notify"
}

// stripTime removes the matched time token from the phrase, leaving prompt text.
func stripTime(s, token string) string { return strings.Replace(s, token, " ", 1) }

// cleanPrompt strips the leading remind-verb scaffolding so the prompt is the
// bare task ("提醒我检查烧录日志" → "检查烧录日志").
func cleanPrompt(s string) string {
	s = strings.TrimSpace(s)
	for _, lead := range []string{"提醒我一下", "提醒我", "提醒一下", "提醒", "记得", "帮我"} {
		s = strings.TrimPrefix(strings.TrimSpace(s), lead)
	}
	return normalize(strings.TrimSpace(s))
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}
