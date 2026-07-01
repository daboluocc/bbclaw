package command

import "testing"

func TestParseExactCommands(t *testing.T) {
	cases := []struct {
		in   string
		kind string
	}{
		{"停止", KindCancel},
		{"停止。", KindCancel},
		{" 取消 ", KindCancel},
		{"stop", KindCancel},
		{"STOP", KindCancel},
		{"新对话", KindNewSession},
		{"重新开始！", KindNewSession},
		{"状态", KindStatus},
		{"现在怎样？", KindStatus},
		{"看提醒", KindReminderList},
	}
	for _, c := range cases {
		got := Parse(c.in, "voice")
		if got == nil {
			t.Fatalf("Parse(%q) = nil, want kind %s", c.in, c.kind)
		}
		if got.Kind != c.kind {
			t.Errorf("Parse(%q).Kind = %s, want %s", c.in, got.Kind, c.kind)
		}
		if got.Source != "voice" {
			t.Errorf("Parse(%q).Source = %s, want voice", c.in, got.Source)
		}
	}
}

func TestParseOrdinaryTurnIsNil(t *testing.T) {
	for _, in := range []string{
		"帮我把这个函数重构一下",
		"明天去开会", // 时间但无提醒动词、无 HH:mm → 普通 turn
		"",
		"   ",
		"讲个笑话",
	} {
		if got := Parse(in, "voice"); got != nil {
			t.Errorf("Parse(%q) = %+v, want nil (ordinary turn)", in, got)
		}
	}
}

func TestParseReminderDelay(t *testing.T) {
	cases := []struct {
		in     string
		delay  string
		prompt string
	}{
		{"30分钟后提醒我检查烧录日志", "30m", "检查烧录日志"},
		{"30 分钟后提醒我看日志", "30m", "看日志"},
		{"2小时后提醒我看 issue", "2h", "看 issue"},
		{"半小时后提醒我", "", ""}, // "半" 不是数字 → 仅 remind verb，delay 缺省
	}
	for _, c := range cases {
		got := Parse(c.in, "voice")
		if got == nil {
			t.Fatalf("Parse(%q) = nil, want reminder.create", c.in)
		}
		if got.Kind != KindReminderCreate {
			t.Fatalf("Parse(%q).Kind = %s, want reminder.create", c.in, got.Kind)
		}
		if c.delay != "" && got.Args["delay"] != c.delay {
			t.Errorf("Parse(%q) delay = %q, want %q", c.in, got.Args["delay"], c.delay)
		}
		if c.prompt != "" && got.Args["prompt"] != c.prompt {
			t.Errorf("Parse(%q) prompt = %q, want %q", c.in, got.Args["prompt"], c.prompt)
		}
	}
}

func TestParseReminderTomorrow(t *testing.T) {
	got := Parse("明天9:30提醒我汇报进度", "voice")
	if got == nil || got.Kind != KindReminderCreate {
		t.Fatalf("Parse tomorrow = %+v, want reminder.create", got)
	}
	if got.Args["at"] != "tomorrow 09:30" {
		t.Errorf("at = %q, want tomorrow 09:30", got.Args["at"])
	}
	if got.Args["prompt"] != "汇报进度" {
		t.Errorf("prompt = %q, want 汇报进度", got.Args["prompt"])
	}
}

func TestParseReminderMode(t *testing.T) {
	cases := []struct {
		in       string
		wantMode string
	}{
		{"30分钟后提醒我喝水", "notify"},           // plain alarm
		{"30分钟后提醒我看日志", "notify"},          // bare 看 stays an alarm
		{"半小时后提醒我帮我查烧录日志有没有报错", "task"},    // 帮我 → delegated task
		{"2小时后提醒我汇报一下 open issue", "task"}, // 汇报 → task
		{"明天9点提醒我总结昨天的提交", "task"},         // 总结 → task
	}
	for _, c := range cases {
		got := Parse(c.in, "voice")
		if got == nil || got.Kind != KindReminderCreate {
			t.Fatalf("Parse(%q) = %+v, want reminder.create", c.in, got)
		}
		if got.Args["mode"] != c.wantMode {
			t.Errorf("Parse(%q) mode = %q, want %q", c.in, got.Args["mode"], c.wantMode)
		}
	}
}

func TestParseReminderTomorrowChinesePoint(t *testing.T) {
	// "明天9点" → minutes default 00
	got := Parse("明天9点提醒我", "voice")
	if got == nil || got.Args["at"] != "tomorrow 09:00" {
		t.Fatalf("Parse(明天9点) = %+v, want at tomorrow 09:00", got)
	}
}
