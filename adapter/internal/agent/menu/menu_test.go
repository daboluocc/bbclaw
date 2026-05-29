package menu

import (
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

func TestDrivers(t *testing.T) {
	in := []agent.DriverInfo{{Name: "opencode"}, {Name: "claude-code"}, {Name: "ollama"}}
	m := Drivers(in, "claude-code")

	if m.ID != "drivers" || m.MenuVersion != Version || m.Title == "" {
		t.Fatalf("menu header wrong: %+v", m)
	}
	want := []string{"claude-code", "ollama", "opencode"} // sorted
	if len(m.Rows) != 3 {
		t.Fatalf("rows=%d want 3", len(m.Rows))
	}
	for i, r := range m.Rows {
		if r.ID != want[i] || r.Label != want[i] {
			t.Errorf("row %d = %q want %q", i, r.ID, want[i])
		}
		if r.Action.Type != "set_driver" || r.Action.Driver != want[i] {
			t.Errorf("row %d action = %+v", i, r.Action)
		}
	}
	if m.Rows[0].Marker != "active" || m.SelectedIndex != 0 {
		t.Errorf("active marker/cursor wrong: marker=%q sel=%d", m.Rows[0].Marker, m.SelectedIndex)
	}
	if m.Rows[1].Marker != "" {
		t.Errorf("non-active row should have empty marker, got %q", m.Rows[1].Marker)
	}
	if got := Drivers(nil, ""); len(got.Rows) != 0 || got.EmptyText == "" {
		t.Errorf("empty drivers menu wrong: %+v", got)
	}
}

func TestModels(t *testing.T) {
	in := []agent.ModelInfo{{ID: "m1", Label: "Model One"}, {ID: "m2"}}
	m := Models("claude-code", in, "m2")

	if m.ID != "models" || m.MenuVersion != Version {
		t.Fatalf("menu header wrong: %+v", m)
	}
	if m.Rows[0].Label != "Model One" {
		t.Errorf("row0 label = %q want Model One", m.Rows[0].Label)
	}
	if m.Rows[1].Label != "m2" { // fallback to ID
		t.Errorf("row1 label = %q want m2 (id fallback)", m.Rows[1].Label)
	}
	if m.Rows[1].Marker != "active" || m.SelectedIndex != 1 {
		t.Errorf("active marker/cursor wrong: marker=%q sel=%d", m.Rows[1].Marker, m.SelectedIndex)
	}
	a := m.Rows[0].Action
	if a.Type != "set_model" || a.Driver != "claude-code" || a.Model != "m1" {
		t.Errorf("row0 action = %+v", a)
	}
	if got := Models("claude-code", nil, ""); len(got.Rows) != 0 || got.EmptyText == "" {
		t.Errorf("empty models menu wrong: %+v", got)
	}
}

func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "刚刚"},
		{5 * time.Minute, "5 分钟前"},
		{3 * time.Hour, "3 小时前"},
		{2 * 24 * time.Hour, "2 天前"},
	}
	for _, c := range cases {
		if got := FormatRelativeTime(now.Add(-c.d), now); got != c.want {
			t.Errorf("d=%s got %q want %q", c.d, got, c.want)
		}
	}
	if got := FormatRelativeTime(time.Time{}, now); got != "" {
		t.Errorf("zero time → %q want empty", got)
	}
	if got := FormatRelativeTime(now.Add(-30*24*time.Hour), now); got == "" || strings.Contains(got, "前") {
		t.Errorf(">7d should format as a date, got %q", got)
	}
}

func TestSessions(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	items := []SessionItem{
		{ID: "ls-1", Title: "Auth", Driver: "claude-code", CwdName: "proj", LastUsedAt: now.Add(-5 * time.Minute)},
		{ID: "ls-2", Title: "", Driver: "ollama", LastUsedAt: now.Add(-2 * time.Hour)},
	}
	m := Sessions(items, "ls-2", now)
	if m.ID != "sessions" || len(m.Rows) != 3 { // 2 sessions + "+ 新建"
		t.Fatalf("menu=%+v", m)
	}
	if m.Rows[0].Secondary != "claude-code · 5 分钟前 · proj" {
		t.Errorf("row0 secondary=%q", m.Rows[0].Secondary)
	}
	if m.Rows[1].Label != "(无标题)" {
		t.Errorf("empty-title fallback wrong: %q", m.Rows[1].Label)
	}
	if m.Rows[1].Marker != "active" || m.SelectedIndex != 1 {
		t.Errorf("ls-2 should be active+cursor: marker=%q sel=%d", m.Rows[1].Marker, m.SelectedIndex)
	}
	if m.Rows[0].Action.Type != "select_session" || m.Rows[0].Action.SessionID != "ls-1" {
		t.Errorf("row0 action=%+v", m.Rows[0].Action)
	}
	last := m.Rows[2]
	if last.ID != "__new__" || last.Action.Type != "open_menu" || last.Action.MenuID != "cwd" {
		t.Errorf("new row=%+v", last)
	}
	if em := Sessions(nil, "", now); len(em.Rows) != 1 || em.Rows[0].ID != "__new__" {
		t.Errorf("empty sessions menu should be just the new row: %+v", em.Rows)
	}
}

func TestCwd(t *testing.T) {
	m := Cwd([]string{"a", "b"})
	if len(m.Rows) != 2 {
		t.Fatalf("rows=%d", len(m.Rows))
	}
	if m.Rows[0].Action.Type != "create_session" || m.Rows[0].Action.Cwd != "a" {
		t.Errorf("row0 action=%+v", m.Rows[0].Action)
	}
	em := Cwd(nil)
	if len(em.Rows) != 1 || em.Rows[0].ID != "__default__" || em.Rows[0].Action.Cwd != "" {
		t.Errorf("empty pool should offer default workspace: %+v", em.Rows)
	}
}
