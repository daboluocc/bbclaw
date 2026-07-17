package claudecode

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	claudeengine "github.com/zhoushoujianwork/agent-runner/engine/claude"
	agentrunner "github.com/zhoushoujianwork/agent-runner/runner"
)

// parseStreamJSON is a test-compat shim for the pre-agent-runner parser
// tests: it feeds the canned transcript through the SAME agent-runner
// protocol parser production uses, then through the driver's translation
// layer — a contract test of the full parse+translate path. Malformed lines
// are skipped (in production agent-runner fails the turn on them instead of
// tolerating).
func parseStreamJSON(r io.Reader, s *session, log *obs.Logger) {
	d := New(Options{}, log)
	protocol, err := claudeengine.New("claude").NewSession(agentrunner.SessionRequest{})
	if err != nil {
		panic(err)
	}
	if s.seenToolUse == nil { // robust to directly-constructed sessions
		s.seenToolUse = make(map[string]bool)
	}
	cap := &stderrCapture{}
	toolUseNames := make(map[string]string)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		step, err := protocol.ParseLine(sc.Bytes())
		if err != nil {
			continue
		}
		for _, ev := range step.Events {
			d.consumeRunnerEvent(s, cap, toolUseNames, ev)
		}
	}
}

func runTranscript(t *testing.T, _ *Driver, s *session, transcript string) {
	t.Helper()
	parseStreamJSON(strings.NewReader(transcript), s, obs.NewLogger())
}

func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	return New(Options{}, obs.NewLogger())
}

// Test the parse+translate path with a canned transcript that exercises
// init / assistant text / tool_use / result.
func TestTranslateStream(t *testing.T) {
	const transcript = `{"type":"system","subtype":"init","session_id":"abc-123","model":"claude-sonnet-4-6"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":" world"},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls -la"}}]}}
{"type":"result","subtype":"success","result":"Hello world","usage":{"input_tokens":12,"output_tokens":3}}
`

	d := newTestDriver(t)
	s := &session{
		id:          "sid-test",
		events:      make(chan agent.Event, 16),
		rootCtx:     context.Background(),
		seenToolUse: make(map[string]bool),
	}

	runTranscript(t, d, s, transcript)
	close(s.events)

	var got []agent.Event
	for e := range s.events {
		got = append(got, e)
	}

	if len(got) != 5 {
		t.Fatalf("want 5 events (1 session_init + 2 text + 1 tool_call + 1 tokens), got %d: %+v", len(got), got)
	}

	if got[0].Type != agent.EvSessionInit || got[0].Text != "abc-123" {
		t.Errorf("event 0: want EvSessionInit 'abc-123', got %+v", got[0])
	}
	if got[1].Type != agent.EvText || got[1].Text != "Hello" {
		t.Errorf("event 1: want EvText 'Hello', got %+v", got[1])
	}
	if got[2].Type != agent.EvText || got[2].Text != " world" {
		t.Errorf("event 2: want EvText ' world', got %+v", got[2])
	}
	if got[3].Type != agent.EvToolCall || got[3].Tool == nil {
		t.Fatalf("event 3: want EvToolCall, got %+v", got[3])
	}
	if got[3].Tool.Tool != "Bash" || got[3].Tool.Hint != "ls -la" || got[3].Tool.ID != "tu_1" {
		t.Errorf("tool_call: want tool=Bash hint='ls -la' id=tu_1, got %+v", got[3].Tool)
	}
	if got[4].Type != agent.EvTokens || got[4].Tokens == nil {
		t.Fatalf("event 4: want EvTokens, got %+v", got[4])
	}
	if got[4].Tokens.In != 12 || got[4].Tokens.Out != 3 {
		t.Errorf("tokens: want in=12 out=3, got %+v", got[4].Tokens)
	}

	// Resume ID should have been captured from the init event.
	if s.resumeID != "abc-123" {
		t.Errorf("resumeID: want 'abc-123', got %q", s.resumeID)
	}
}

// TestTranslateStreamDropsPartialFrames verifies that stream_event partial
// frames (text deltas / content_block_start replays emitted because agent-
// runner passes --include-partial-messages) are NOT double-surfaced: only the
// full assistant frame produces device events.
func TestTranslateStreamDropsPartialFrames(t *testing.T) {
	const transcript = `{"type":"stream_event","session_id":"abc","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}
{"type":"stream_event","session_id":"abc","event":{"type":"content_block_start","content_block":{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var texts, tools int
	for e := range s.events {
		switch e.Type {
		case agent.EvText:
			texts++
		case agent.EvToolCall:
			tools++
		}
	}
	if texts != 1 {
		t.Errorf("want exactly 1 EvText (deltas dropped), got %d", texts)
	}
	if tools != 1 {
		t.Errorf("want exactly 1 EvToolCall (partial frame dropped), got %d", tools)
	}
}

// TestTranslateStreamToolUseEmitsEvToolCall feeds a transcript with a single
// tool_use block whose command is longer than the toolHintMaxLen truncation
// limit and verifies exactly one EvToolCall event is emitted with the expected
// tool name and truncated hint.
func TestTranslateStreamToolUseEmitsEvToolCall(t *testing.T) {
	longCmd := strings.Repeat("x", toolHintMaxLen+40)
	transcript := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_42","name":"Bash","input":{"command":"` + longCmd + `"}}]}}` + "\n"

	d := newTestDriver(t)
	s := &session{
		id:          "sid-test",
		events:      make(chan agent.Event, 4),
		rootCtx:     context.Background(),
		seenToolUse: make(map[string]bool),
	}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var tools []agent.Event
	for e := range s.events {
		if e.Type == agent.EvToolCall {
			tools = append(tools, e)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("want exactly 1 EvToolCall, got %d", len(tools))
	}
	ev := tools[0]
	if ev.Tool == nil {
		t.Fatalf("Tool payload is nil")
	}
	if ev.Tool.Tool != "Bash" {
		t.Errorf("tool name: want Bash, got %q", ev.Tool.Tool)
	}
	if ev.Tool.ID != "tu_42" {
		t.Errorf("tool id: want tu_42, got %q", ev.Tool.ID)
	}
	// Hint must be truncated: toolHintMaxLen chars + the ellipsis suffix.
	wantPrefix := strings.Repeat("x", toolHintMaxLen)
	if !strings.HasPrefix(ev.Tool.Hint, wantPrefix) {
		t.Errorf("hint prefix: want %d x's, got %q", toolHintMaxLen, ev.Tool.Hint)
	}
	if !strings.HasSuffix(ev.Tool.Hint, "…") {
		t.Errorf("hint should end with ellipsis on truncation, got %q", ev.Tool.Hint)
	}
}

// TestSummarizeToolInput covers the small switch in summarizeToolInput so
// that changes to the field mapping are caught by a unit test rather than a
// manual playground session.
func TestSummarizeToolInput(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash", "Bash", `{"command":"ls -la /tmp"}`, "ls -la /tmp"},
		{"bash trims whitespace", "Bash", `{"command":"   echo hi   "}`, "echo hi"},
		{"edit file_path", "Edit", `{"file_path":"/a/b.go","old_string":"x"}`, "/a/b.go"},
		{"write file_path", "Write", `{"file_path":"/a/b.go","content":"..."}`, "/a/b.go"},
		{"read file_path", "Read", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"unknown tool returns empty", "Weirdo", `{"x":1}`, ""},
		{"empty raw returns empty", "Bash", ``, ""},
		{"malformed json returns empty", "Bash", `{not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeToolInput(tc.tool, []byte(tc.input))
			if got != tc.want {
				t.Errorf("summarizeToolInput(%s, %s): want %q, got %q", tc.tool, tc.input, tc.want, got)
			}
		})
	}
}

func TestCapabilitiesStable(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	caps := d.Capabilities()
	if caps.ToolApproval {
		t.Error("must not advertise ToolApproval until OnPermission is wired")
	}
	if !caps.Resume || !caps.Streaming {
		t.Error("Resume and Streaming must be true")
	}
	if caps.MaxInputBytes <= 0 {
		t.Error("MaxInputBytes must be positive")
	}
}

func TestApproveReturnsUnsupported(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	err := d.Approve("any", "t1", agent.DecisionOnce)
	if err != agent.ErrUnsupported {
		t.Errorf("want ErrUnsupported, got %v", err)
	}
}

func TestDriverName(t *testing.T) {
	d := New(Options{}, obs.NewLogger())
	if d.Name() != "claude-code" {
		t.Errorf("Name: want 'claude-code', got %q", d.Name())
	}
}

// TestCLISessionExists verifies the filesystem check used by the agent proxy
// to skip doomed --resume attempts. The driver should return false when no
// matching JSONL exists and true when one is present.
func TestCLISessionExists(t *testing.T) {
	d := New(Options{}, obs.NewLogger())

	// Point CLAUDE_SESSIONS_DIR at a temp dir so we don't touch ~/.claude.
	// The driver derives projectsDir as filepath.Join(filepath.Dir(sessionsDir), "projects").
	sessionsDir := t.TempDir()
	t.Setenv("CLAUDE_SESSIONS_DIR", sessionsDir)

	// No projects dir yet → false, no panic.
	if d.CLISessionExists("abc-123") {
		t.Error("CLISessionExists: want false when projects dir is absent")
	}
	if d.CLISessionExists("") {
		t.Error("CLISessionExists: want false for empty id")
	}

	// Create a fake JSONL transcript under projects/<project>/abc-123.jsonl.
	projectsDir := filepath.Join(filepath.Dir(sessionsDir), "projects")
	projectSubdir := filepath.Join(projectsDir, "-tmp-myproject")
	if err := os.MkdirAll(projectSubdir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	jsonlPath := filepath.Join(projectSubdir, "abc-123.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !d.CLISessionExists("abc-123") {
		t.Error("CLISessionExists: want true when JSONL file exists")
	}
	// A different id in the same project dir should still return false.
	if d.CLISessionExists("xyz-999") {
		t.Error("CLISessionExists: want false for id with no matching file")
	}
}

// TestBuildRequest verifies the per-turn agent-runner request assembly: model
// override, system prompt, mcp-config, session continuity (resume vs new
// session id) and driver extra args pass-through.
func TestBuildRequest(t *testing.T) {
	s := &session{
		id:           "sid-1",
		model:        "claude-opus-4-8",
		systemPrompt: "be brief",
		mcpConfig:    "/cfg/butler-mcp.json",
		cwd:          "/workspace",
	}
	req := buildRequest(s, "hello", []string{"--foo", "bar"}, map[string]string{"X": "1"})
	if req.Prompt != "hello" || req.WorkDir != "/workspace" {
		t.Errorf("prompt/cwd: %+v", req)
	}
	if req.Model != "claude-opus-4-8" || req.AppendSystemPrompt != "be brief" || req.MCPConfig != "/cfg/butler-mcp.json" {
		t.Errorf("session flags not mapped: %+v", req)
	}
	if len(req.ExtraArgs) != 2 || req.ExtraArgs[0] != "--foo" {
		t.Errorf("extra args: %v", req.ExtraArgs)
	}
	if req.Env["X"] != "1" {
		t.Errorf("env: %v", req.Env)
	}
	// New session: adapter uuid becomes the CLI session id.
	if req.SessionID != "" || req.NewSessionID != "sid-1" {
		t.Errorf("new session continuity: %+v", req)
	}
	// Resumed session: prior resume id wins.
	s.resumeID = "resume-9"
	req = buildRequest(s, "again", nil, nil)
	if req.SessionID != "resume-9" || req.NewSessionID != "" {
		t.Errorf("resume continuity: %+v", req)
	}
}

// TestDefaultModelMatchesCatalog locks the single-source-of-truth: the runtime
// --model fallback (defaultModelID) must equal the catalog's factory default
// (claudeCodeModels[0]) and be a model the catalog actually offers (regression
// for the old stale claude-sonnet-4-5 that wasn't in the list).
func TestDefaultModelMatchesCatalog(t *testing.T) {
	def := defaultModelID()
	if def == "" {
		t.Fatal("defaultModelID() empty")
	}
	if def != claudeCodeModels[0].ID {
		t.Errorf("defaultModelID()=%q want catalog[0]=%q", def, claudeCodeModels[0].ID)
	}
	found := false
	for _, m := range claudeCodeModels {
		if m.ID == def {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default model %q is not in the catalog", def)
	}
}

// ─── dispatch_status tests (ADR-021-firmware-ui §1.2) ───────────────────────

// TestTranslateStreamDispatchStarted verifies that mcp__bbclaw__dispatch tool_use
// frames emit EvDispatchStatus(started) instead of EvToolCall.
func TestTranslateStreamDispatchStarted(t *testing.T) {
	const transcript = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_disp1","name":"mcp__bbclaw__dispatch","input":{"cwd":"bbclaw","prompt":"重构 auth"}}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Type != agent.EvDispatchStatus {
		t.Fatalf("want EvDispatchStatus, got %v", ev.Type)
	}
	if ev.Dispatch == nil {
		t.Fatal("Dispatch field is nil")
	}
	if ev.Dispatch.Phase != "started" {
		t.Errorf("want phase=started, got %q", ev.Dispatch.Phase)
	}
	if ev.Dispatch.TaskID != "tu_disp1" {
		t.Errorf("want taskId=tu_disp1, got %q", ev.Dispatch.TaskID)
	}
	if ev.Dispatch.Cwd != "bbclaw" {
		t.Errorf("want cwd=bbclaw, got %q", ev.Dispatch.Cwd)
	}
}

// TestTranslateStreamNonDispatchMCPToolStillEvToolCall verifies that other
// mcp__bbclaw__* tools (list_projects, task_status, task_result) are NOT
// treated as dispatch and still emit EvToolCall.
func TestTranslateStreamNonDispatchMCPToolStillEvToolCall(t *testing.T) {
	const transcript = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_ls","name":"mcp__bbclaw__list_projects","input":{}}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if evs[0].Type != agent.EvToolCall {
		t.Errorf("want EvToolCall for non-dispatch MCP tool, got %v", evs[0].Type)
	}
}

// TestTranslateStreamDispatchToolResult verifies that a tool_result frame for
// mcp__bbclaw__dispatch emits EvDispatchStatus with the parsed phase/elapsedMs.
// The transcript must contain the tool_use frame first so toolUseNames is populated.
func TestTranslateStreamDispatchToolResult(t *testing.T) {
	const transcript = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_disp1","name":"mcp__bbclaw__dispatch","input":{"cwd":"bbclaw","prompt":"重构 auth"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_disp1","content":"{\"status\":\"done\",\"taskId\":\"tu_disp1\",\"elapsedMs\":3200}"}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	// expect: 1 EvDispatchStatus(started) + 1 EvDispatchStatus(done)
	if len(evs) != 2 {
		t.Fatalf("want 2 events (started+done), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != agent.EvDispatchStatus || evs[0].Dispatch.Phase != "started" {
		t.Errorf("event 0: want EvDispatchStatus(started), got %+v", evs[0])
	}
	ev := evs[1]
	if ev.Type != agent.EvDispatchStatus {
		t.Fatalf("event 1: want EvDispatchStatus, got %v", ev.Type)
	}
	if ev.Dispatch.Phase != "done" {
		t.Errorf("want phase=done, got %q", ev.Dispatch.Phase)
	}
	if ev.Dispatch.ElapsedMs != 3200 {
		t.Errorf("want elapsedMs=3200, got %d", ev.Dispatch.ElapsedMs)
	}
}

// TestTranslateStreamDispatchAsyncPhase verifies the async phase parsing.
func TestTranslateStreamDispatchAsyncPhase(t *testing.T) {
	const transcript = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_async1","name":"mcp__bbclaw__dispatch","input":{"cwd":"proj","prompt":"大重构"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_async1","content":"{\"status\":\"async\",\"taskId\":\"tu_async1\"}"}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if evs[1].Dispatch.Phase != "async" {
		t.Errorf("want phase=async, got %q", evs[1].Dispatch.Phase)
	}
}

// TestTranslateStreamDispatchErrorPhase verifies the error phase and ErrorMsg.
func TestTranslateStreamDispatchErrorPhase(t *testing.T) {
	const transcript = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu_err1","name":"mcp__bbclaw__dispatch","input":{"cwd":"proj","prompt":"lint"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_err1","content":"{\"status\":\"error\",\"taskId\":\"tu_err1\",\"error\":\"timeout\"}"}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if evs[1].Dispatch.Phase != "error" {
		t.Errorf("want phase=error, got %q", evs[1].Dispatch.Phase)
	}
	if evs[1].Dispatch.ErrorMsg != "timeout" {
		t.Errorf("want error=timeout, got %q", evs[1].Dispatch.ErrorMsg)
	}
}

// TestTranslateStreamNonMCPToolResultIgnored verifies that a tool_result for a
// non-mcp__bbclaw__dispatch tool_use_id is silently ignored (no events emitted).
func TestTranslateStreamNonMCPToolResultIgnored(t *testing.T) {
	const transcript = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"bash_123","content":"exit 0"}]}}
`
	d := newTestDriver(t)
	s := &session{id: "sid-test", events: make(chan agent.Event, 16), rootCtx: context.Background(), seenToolUse: make(map[string]bool)}
	runTranscript(t, d, s, transcript)
	close(s.events)

	var evs []agent.Event
	for e := range s.events {
		evs = append(evs, e)
	}
	if len(evs) != 0 {
		t.Errorf("want 0 events for non-dispatch tool_result, got %d: %+v", len(evs), evs)
	}
}

// TestInterruptKillsTurnKeepsSession spawns a fake `claude` that prints an
// init frame then blocks, interrupts it mid-turn, and verifies the barge-in
// contract (ADR-028 §2.5.1):
//  1. the turn ends with EvInterrupted + EvTurnEnd and NO EvError
//  2. the session survives (still registered, resumeID preserved)
//  3. Interrupt with no in-flight turn is a no-op
func TestInterruptKillsTurnKeepsSession(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// `exec sleep` replaces the shell so SIGTERM lands on the blocker directly
	// and the stdout pipe closes immediately on death. agent-runner delivers
	// the prompt via stdin (stream-json) and closes it right away; the script
	// never reads stdin, which is fine.
	script := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init","session_id":"int-1","model":"m"}'` + "\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := New(Options{Bin: bin}, obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{Cwd: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := d.Events(sid)
	sendDone := make(chan error, 1)
	go func() { sendDone <- d.Send(sid, "hello") }()

	// Wait for the init event so we know the subprocess is alive mid-turn.
	select {
	case ev := <-events:
		if ev.Type != agent.EvSessionInit {
			t.Fatalf("first event: want EvSessionInit, got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for EvSessionInit")
	}

	if err := d.Interrupt(sid); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	var sawInterrupted, sawError, sawTurnEnd bool
collect:
	for {
		select {
		case ev := <-events:
			switch ev.Type {
			case agent.EvInterrupted:
				sawInterrupted = true
			case agent.EvError:
				sawError = true
				t.Logf("unexpected EvError: %s", ev.Text)
			case agent.EvTurnEnd:
				sawTurnEnd = true
				break collect
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for EvTurnEnd after Interrupt")
		}
	}
	if !sawInterrupted {
		t.Error("want EvInterrupted before EvTurnEnd")
	}
	if sawError {
		t.Error("interrupted turn must not emit EvError")
	}
	if !sawTurnEnd {
		t.Error("want EvTurnEnd")
	}
	if err := <-sendDone; err != nil {
		t.Errorf("Send after interrupt: want nil, got %v", err)
	}

	// Session must survive with its resume id for the next --resume turn.
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		t.Fatal("session destroyed by Interrupt; must survive for --resume")
	}
	if s.resumeID != "int-1" {
		t.Errorf("resumeID: want 'int-1', got %q", s.resumeID)
	}

	// No in-flight turn now → Interrupt is a no-op.
	if err := d.Interrupt(sid); err != nil {
		t.Errorf("idle Interrupt: want nil, got %v", err)
	}
	_ = d.Stop(sid)
}

// TestSendPromptNotInArgv spawns a fake `claude` that dumps its argv and
// verifies the prompt travels via stdin (stream-json), never argv — the
// security property agent-runner enforces (the old driver leaked it via -p).
func TestSendPromptNotInArgv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	argsFile := filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > ` + argsFile + "\n" +
		`cat > /dev/null` + "\n" + // wait for stdin EOF like a stream-json consumer
		`echo '{"type":"system","subtype":"init","session_id":"argv-1"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"argv-1"}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := New(Options{Bin: bin}, obs.NewLogger())
	sid, err := d.Start(context.Background(), agent.StartOpts{Cwd: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		for range d.Events(sid) {
		}
	}()
	if err := d.Send(sid, "super secret prompt"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super secret prompt") {
		t.Fatalf("prompt leaked into argv: %s", data)
	}
	if !strings.Contains(string(data), "--session-id") {
		t.Errorf("first turn should pass --session-id, argv: %s", data)
	}
	_ = d.Stop(sid)
}
