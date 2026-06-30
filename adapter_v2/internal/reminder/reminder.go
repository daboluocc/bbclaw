// Package reminder is adapter_v2's one-shot reminder store + scheduler (ADR-042
// §3). A reminder is a future prompt the adapter runs ON ITS OWN — no PTT — and
// speaks back, like an SMS reminder. P0 does only timer.once; periodic report.cron
// is P1.
//
// Split of responsibilities:
//   - Store  — persist/list/cancel reminders at <DataDir>/reminders.json (atomic
//     write, mirrors projectstore/settingsstore).
//   - Resolve — turn the command router's parsed args ({"delay":"30m"} or
//     {"at":"tomorrow 21:30"}) into an absolute RunAt. Time parsing is the risky
//     bit (ADR-042 §9), so it is isolated and unit-tested with an injected clock.
//   - Scheduler — a single goroutine that fires due reminders through an Injector.
//
// Firing belongs to the CREATING session (ADR-042 §3 decision): the Reminder
// carries a Target, and the Injector routes the prompt back to that device's live
// bridge; offline targets defer to the notify outbox (M3).
package reminder

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind values. P0 is "once"; "cron" is P1.
const (
	KindOnce = "once"
)

// State machine for a reminder's lifecycle.
const (
	StateScheduled = "scheduled"
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// Target binds a reminder to the session that created it, so firing routes back
// there (ADR-042 §3). Empty fields mean "the default device/session".
type Target struct {
	DeviceID  string `json:"deviceId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	CwdName   string `json:"cwdName,omitempty"`
}

// Reminder is one scheduled prompt.
type Reminder struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // KindOnce (P0)
	Title     string    `json:"title,omitempty"`
	Prompt    string    `json:"prompt"`
	RunAt     time.Time `json:"runAt"`
	Target    Target    `json:"target,omitempty"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
}

// defaultPrompt is used when the user set a time but no task ("30 分钟后提醒我").
const defaultPrompt = "到点提醒：你设置的提醒时间到了。"

// Resolve turns command-router args into an absolute RunAt relative to now.
// Recognised keys (ADR-042 §2.4 grammar): "delay" (Go duration like "30m"/"2h")
// and "at" ("tomorrow HH:MM"). Exactly one must be present and parseable; the
// resulting time must be in the future.
func Resolve(args map[string]string, now time.Time) (runAt time.Time, prompt string, err error) {
	prompt = strings.TrimSpace(args["prompt"])
	if prompt == "" {
		prompt = defaultPrompt
	}
	switch {
	case args["delay"] != "":
		d, perr := time.ParseDuration(args["delay"])
		if perr != nil {
			return time.Time{}, "", fmt.Errorf("reminder: bad delay %q: %w", args["delay"], perr)
		}
		if d <= 0 {
			return time.Time{}, "", errors.New("reminder: delay must be positive")
		}
		return now.Add(d), prompt, nil
	case args["at"] != "":
		t, perr := parseAt(args["at"], now)
		if perr != nil {
			return time.Time{}, "", perr
		}
		if !t.After(now) {
			return time.Time{}, "", fmt.Errorf("reminder: time %s is not in the future", args["at"])
		}
		return t, prompt, nil
	default:
		return time.Time{}, "", errors.New("reminder: no time given (need delay or at)")
	}
}

// parseAt handles the P0 absolute form "tomorrow HH:MM" in now's location. Other
// forms are rejected (P1 widens this) rather than guessed, per ADR-042 §9.
func parseAt(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, "tomorrow ")
	if !ok {
		return time.Time{}, fmt.Errorf("reminder: unsupported time form %q", s)
	}
	hm := strings.SplitN(strings.TrimSpace(rest), ":", 2)
	if len(hm) != 2 {
		return time.Time{}, fmt.Errorf("reminder: bad clock %q", rest)
	}
	var hh, mm int
	if _, err := fmt.Sscanf(hm[0], "%d", &hh); err != nil {
		return time.Time{}, fmt.Errorf("reminder: bad hour %q", hm[0])
	}
	if _, err := fmt.Sscanf(hm[1], "%d", &mm); err != nil {
		return time.Time{}, fmt.Errorf("reminder: bad minute %q", hm[1])
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return time.Time{}, fmt.Errorf("reminder: clock out of range %02d:%02d", hh, mm)
	}
	d := now.AddDate(0, 0, 1)
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, now.Location()), nil
}

// ConfirmText is the spoken acknowledgement after a reminder is created, e.g.
// "已设置 30 分钟后提醒：检查烧录日志". It reads the delta from now so it works for
// both delay and absolute forms.
func ConfirmText(r Reminder, now time.Time) string {
	when := humanizeUntil(r.RunAt.Sub(now))
	task := strings.TrimSpace(r.Prompt)
	if task == "" || task == defaultPrompt {
		return "已设置" + when + "提醒。"
	}
	return "已设置" + when + "提醒：" + task
}

// humanizeUntil renders a positive duration as a coarse Chinese phrase for TTS.
func humanizeUntil(d time.Duration) string {
	if d < time.Minute {
		return "马上"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟后", int(d.Minutes()+0.5))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%d 小时后", h)
		}
		return fmt.Sprintf("%d 小时%d 分钟后", h, m)
	}
	return fmt.Sprintf("%d 天后", int(d.Hours()/24))
}
