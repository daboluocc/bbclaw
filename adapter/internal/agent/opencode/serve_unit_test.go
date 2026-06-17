package opencode

import (
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

func TestCheckVersion(t *testing.T) {
	cases := []struct {
		ver string
		ok  bool
	}{
		{"1.15.0", true},
		{"1.15.1", true},
		{"1.20.3", true},
		{"1.29.99", true},
		{"1.14.9", false}, // below min minor
		{"1.30.0", false}, // at exclusive max
		{"2.0.0", false},  // wrong major
		{"0.19.2", false}, // wrong major
		{"unknown", true}, // unparseable → allowed (reachability already proven)
		{"v1.16.0", true}, // leading v tolerated
	}
	for _, c := range cases {
		err := checkVersion(c.ver)
		if (err == nil) != c.ok {
			t.Errorf("checkVersion(%q): ok=%v err=%v", c.ver, c.ok, err)
		}
	}
}

func TestSplitModel(t *testing.T) {
	cases := []struct {
		in        string
		prov, mod string
		ok        bool
	}{
		{"deepseek/deepseek-v4-pro", "deepseek", "deepseek-v4-pro", true},
		{"anthropic/claude-opus-4-8", "anthropic", "claude-opus-4-8", true},
		{"", "", "", false},
		{"justmodel", "", "", false},
		{"/x", "", "", false},
		{"p/", "", "", false},
	}
	for _, c := range cases {
		p, m, ok := splitModel(c.in)
		if ok != c.ok || p != c.prov || m != c.mod {
			t.Errorf("splitModel(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, p, m, ok, c.prov, c.mod, c.ok)
		}
	}
}

func TestPageBounds(t *testing.T) {
	cases := []struct {
		n, before, limit int
		lo, hi           int
	}{
		{10, 0, 5, 5, 10},  // latest page
		{10, 0, 50, 0, 10}, // limit > n
		{10, 5, 3, 2, 5},   // before cursor, capped
		{10, 5, 50, 0, 5},  // before cursor, limit > window
		{0, 0, 5, 0, 0},    // empty
		{3, 100, 5, 0, 3},  // before beyond n
	}
	for _, c := range cases {
		lo, hi := pageBounds(c.n, c.before, c.limit)
		if lo != c.lo || hi != c.hi {
			t.Errorf("pageBounds(%d,%d,%d) = (%d,%d), want (%d,%d)", c.n, c.before, c.limit, lo, hi, c.lo, c.hi)
		}
	}
}

func TestMapParts(t *testing.T) {
	d := &ServeDriver{registeredMCP: map[string]bool{"bbclaw": true}, dispatchTools: map[string]bool{"bbclaw_dispatch": true}}
	parts := []ocMsgPart{
		{Type: "step-start"},
		{Type: "reasoning", Text: "thinking..."},
		{Type: "tool", Tool: "Bash"},
		{Type: "tool", Tool: "bbclaw_dispatch"}, // butler dispatch → dispatch kind
		{Type: "text", Text: "hello"},
		{Type: "step-finish"},
		{Type: "subtask", Tool: "worker"},
	}
	got := d.mapParts(parts)
	want := []string{"thinking", "tool", "dispatch", "text", "dispatch"}
	if len(got) != len(want) {
		t.Fatalf("mapParts len=%d want=%d (%+v)", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("part[%d].Kind=%q want %q", i, got[i].Kind, k)
		}
	}
}

func TestMapPartsDispatchPopulated(t *testing.T) {
	d := &ServeDriver{dispatchTools: map[string]bool{"bbclaw_dispatch": true}}
	p := ocMsgPart{Type: "tool", Tool: "bbclaw_dispatch"}
	p.State.Input = []byte(`{"cwd":"/proj","prompt":"do the thing"}`)
	p.State.Output = `{"status":"done","taskId":"T1","elapsedMs":4200,"childSessionId":"ses_child"}`
	parts := d.mapParts([]ocMsgPart{p})
	if len(parts) != 1 || parts[0].Kind != "dispatch" || parts[0].Dispatch == nil {
		t.Fatalf("expected one dispatch part, got %+v", parts)
	}
	dp := parts[0].Dispatch
	if dp.ChildSessionID != "ses_child" || dp.Cwd != "/proj" || dp.Title != "do the thing" ||
		dp.Status != "done" || dp.ElapsedMs != 4200 || dp.TaskID != "T1" {
		t.Errorf("dispatch part not fully reconstructed: %+v", dp)
	}
}

func TestIsDispatchTool(t *testing.T) {
	d := &ServeDriver{
		registeredMCP: map[string]bool{"bbclaw": true},
		dispatchTools: map[string]bool{"bbclaw_dispatch": true},
	}
	cases := []struct {
		name string
		want bool
	}{
		{"bbclaw_dispatch", true}, // exact
		{"bbclaw*dispatch", true}, // lenient: registered prefix + "dispatch"
		{"bbclaw_list_projects", false},
		{"Bash", false},
		{"other_dispatch", false}, // "dispatch" but no registered prefix
		{"", false},
	}
	for _, c := range cases {
		if got := d.isDispatchTool(c.name); got != c.want {
			t.Errorf("isDispatchTool(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseDispatchResult(t *testing.T) {
	// bare JSON object
	ds := parseDispatchResult("call-1", `{"status":"done","taskId":"T9","elapsedMs":1234,"childSessionId":"ses_x"}`)
	if ds.Phase != "done" || ds.TaskID != "T9" || ds.ElapsedMs != 1234 || ds.ChildSessionID != "ses_x" {
		t.Errorf("parseDispatchResult object: %+v", ds)
	}
	// running → async; falls back to callID when taskId absent
	ds = parseDispatchResult("call-2", `{"status":"running"}`)
	if ds.Phase != "async" || ds.TaskID != "call-2" {
		t.Errorf("parseDispatchResult running: %+v", ds)
	}
	// empty output → default done with callID
	ds = parseDispatchResult("call-3", "")
	if ds.Phase != "done" || ds.TaskID != "call-3" {
		t.Errorf("parseDispatchResult empty: %+v", ds)
	}
}

func TestBuildServeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "ANTHROPIC_API_KEY=inherited"}
	out := buildServeEnv(base, "CFG", map[string]string{"ANTHROPIC_API_KEY": "scoped", "DEEPSEEK_API_KEY": "dk"})
	// base preserved
	if out[0] != "PATH=/bin" {
		t.Errorf("base not preserved: %v", out)
	}
	// config content appended
	if !containsStr(out, "OPENCODE_CONFIG_CONTENT=CFG") {
		t.Errorf("config content missing: %v", out)
	}
	// providerEnv appended after inherited → overrides (last value wins in exec)
	last := map[string]string{}
	for _, kv := range out {
		if k, v, ok := cut(kv); ok {
			last[k] = v
		}
	}
	if last["ANTHROPIC_API_KEY"] != "scoped" {
		t.Errorf("providerEnv should override inherited: got %q", last["ANTHROPIC_API_KEY"])
	}
	if last["DEEPSEEK_API_KEY"] != "dk" {
		t.Errorf("providerEnv DEEPSEEK missing: %v", last)
	}
	// empty config + nil providerEnv → just base
	if got := buildServeEnv(base, "", nil); len(got) != len(base) {
		t.Errorf("empty extras should equal base: %v", got)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func cut(kv string) (k, v string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

func TestRespawnBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{6, 16 * time.Second},
		{20, 16 * time.Second}, // capped, no overflow
	}
	for _, c := range cases {
		if got := respawnBackoff(c.failures); got != c.want {
			t.Errorf("respawnBackoff(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

func TestApprovalGate(t *testing.T) {
	// Off: Approve is unsupported, Capabilities.ToolApproval false.
	off := &ServeDriver{toolApproval: false, sessions: map[agent.SessionID]*serveSession{}}
	if off.Capabilities().ToolApproval {
		t.Errorf("ToolApproval should be false when gate off")
	}
	if err := off.Approve("any", "tid", agent.DecisionOnce); err != agent.ErrUnsupported {
		t.Errorf("Approve off: got %v, want ErrUnsupported", err)
	}

	// On: Capabilities.ToolApproval true; Approve on an unknown session returns
	// ErrUnknownSession (not ErrUnsupported).
	on := &ServeDriver{toolApproval: true, sessions: map[agent.SessionID]*serveSession{}}
	if !on.Capabilities().ToolApproval {
		t.Errorf("ToolApproval should be true when gate on")
	}
	if err := on.Approve("nope", "tid", agent.DecisionOnce); err != agent.ErrUnknownSession {
		t.Errorf("Approve on/unknown: got %v, want ErrUnknownSession", err)
	}
}

func TestParseDispatchInput(t *testing.T) {
	cwd, title := parseDispatchInput([]byte(`{"cwd":"/proj","prompt":"fix the bug"}`))
	if cwd != "/proj" || title != "fix the bug" {
		t.Errorf("parseDispatchInput new schema: cwd=%q title=%q", cwd, title)
	}
	cwd, title = parseDispatchInput([]byte(`{"project":"/legacy","task":"do thing"}`))
	if cwd != "/legacy" || title != "do thing" {
		t.Errorf("parseDispatchInput legacy schema: cwd=%q title=%q", cwd, title)
	}
}
