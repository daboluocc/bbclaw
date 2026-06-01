package butlermcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// mockRunner is an injectable WorkerRunner. If gate is non-nil Run blocks until
// it's closed; otherwise it returns after delay.
type mockRunner struct {
	result string
	err    error
	delay  time.Duration
	gate   chan struct{}
}

func (m *mockRunner) Run(ctx context.Context, cwd, task string) (string, error) {
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return m.result, m.err
}

func newTestServer(runner WorkerRunner, wait time.Duration) *Server {
	return New(Options{
		Projects:     []Project{{Name: "proj", Cwd: "/p/proj"}, {Name: "other", Cwd: "/p/other"}},
		Runner:       runner,
		DispatchWait: wait,
	})
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return m
}

// ─────────────────────────── protocol ───────────────────────────

func TestServeInitializeAndToolsList(t *testing.T) {
	s := newTestServer(&mockRunner{}, time.Second)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n",
	)
	var out strings.Builder
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// initialize reply + tools/list reply; the notification gets NO reply.
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2 (notification must not reply): %v", len(lines), lines)
	}
	init := parse(t, lines[0])
	res, _ := init["result"].(map[string]any)
	if res["protocolVersion"] != "2024-11-05" {
		t.Errorf("initialize protocolVersion=%v", res["protocolVersion"])
	}
	if si, _ := res["serverInfo"].(map[string]any); si["name"] != "bbclaw-butler" {
		t.Errorf("serverInfo=%v", res["serverInfo"])
	}
	list := parse(t, lines[1])
	lr, _ := list["result"].(map[string]any)
	tools, _ := lr["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools/list returned %d tools, want 4", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"list_projects", "dispatch", "task_status", "task_result"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

// ─────────────────────────── tools ───────────────────────────

func TestListProjects(t *testing.T) {
	s := newTestServer(&mockRunner{}, time.Second)
	text, isErr := s.callTool("list_projects", json.RawMessage(`{}`))
	if isErr {
		t.Fatalf("isErr: %s", text)
	}
	m := parse(t, text)
	ps, _ := m["projects"].([]any)
	if len(ps) != 2 {
		t.Fatalf("projects=%v", m["projects"])
	}
}

func TestDispatchSync(t *testing.T) {
	s := newTestServer(&mockRunner{result: "done it"}, time.Second)
	text, isErr := s.callTool("dispatch", json.RawMessage(`{"project":"proj","task":"do x"}`))
	if isErr {
		t.Fatalf("isErr: %s", text)
	}
	m := parse(t, text)
	if m["status"] != "done" || m["result"] != "done it" {
		t.Fatalf("result=%v", m)
	}
}

func TestDispatchEmptyTask(t *testing.T) {
	s := newTestServer(&mockRunner{}, time.Second)
	text, isErr := s.callTool("dispatch", json.RawMessage(`{"project":"proj","task":"  "}`))
	if !isErr || parse(t, text)["error"] != "EMPTY_TASK" {
		t.Fatalf("want EMPTY_TASK err, got %s (isErr=%v)", text, isErr)
	}
}

func TestDispatchProjectNotAllowed(t *testing.T) {
	s := newTestServer(&mockRunner{result: "x"}, time.Second)
	// Unknown project name.
	if text, isErr := s.callTool("dispatch", json.RawMessage(`{"project":"nope","task":"t"}`)); !isErr || parse(t, text)["error"] != "PROJECT_NOT_ALLOWED" {
		t.Errorf("unknown project: %s (isErr=%v)", text, isErr)
	}
	// Arbitrary cwd not in the allowlist (security: ADR-021 §2).
	if text, isErr := s.callTool("dispatch", json.RawMessage(`{"cwd":"/etc","task":"rm -rf"}`)); !isErr || parse(t, text)["error"] != "PROJECT_NOT_ALLOWED" {
		t.Errorf("arbitrary cwd must be rejected: %s (isErr=%v)", text, isErr)
	}
}

func TestResolveCwd(t *testing.T) {
	s := newTestServer(nil, time.Second)
	if cwd, ok := s.resolveCwd("proj", ""); !ok || cwd != "/p/proj" {
		t.Errorf("by name: %q %v", cwd, ok)
	}
	if cwd, ok := s.resolveCwd("", "/p/other"); !ok || cwd != "/p/other" {
		t.Errorf("by allow-listed cwd: %q %v", cwd, ok)
	}
	if _, ok := s.resolveCwd("", "/tmp/x"); ok {
		t.Error("non-allow-listed cwd should be rejected")
	}
	// Single-project fallback when neither given.
	single := New(Options{Projects: []Project{{Name: "only", Cwd: "/only"}}})
	if cwd, ok := single.resolveCwd("", ""); !ok || cwd != "/only" {
		t.Errorf("single fallback: %q %v", cwd, ok)
	}
	if _, ok := s.resolveCwd("", ""); ok {
		t.Error("no project + multiple configured should be rejected")
	}
}

func TestDispatchAsyncDegradeAndPoll(t *testing.T) {
	gate := make(chan struct{})
	s := newTestServer(&mockRunner{result: "async result", gate: gate}, 30*time.Millisecond)

	// Worker blocks on gate; dispatch must degrade to async after the wait.
	text, isErr := s.callTool("dispatch", json.RawMessage(`{"project":"proj","task":"long"}`))
	if isErr {
		t.Fatalf("isErr: %s", text)
	}
	m := parse(t, text)
	if m["status"] != "running" {
		t.Fatalf("expected running (degraded), got %v", m)
	}
	taskID, _ := m["taskId"].(string)
	if taskID == "" {
		t.Fatal("no taskId on degraded dispatch")
	}

	// status: still running.
	st := parse(t, mustTool(t, s, "task_status", `{"taskId":"`+taskID+`"}`))
	if st["status"] != "running" {
		t.Fatalf("status before completion=%v", st)
	}

	// Let the worker finish, then poll until done.
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := parse(t, mustTool(t, s, "task_status", `{"taskId":"`+taskID+`"}`))
		if st["status"] == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never reached done, last=%v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// result consumed.
	rr := parse(t, mustTool(t, s, "task_result", `{"taskId":"`+taskID+`"}`))
	if rr["status"] != "done" || rr["result"] != "async result" {
		t.Fatalf("task_result=%v", rr)
	}
	// after consuming, the task is gone.
	if text, isErr := s.callTool("task_status", json.RawMessage(`{"taskId":"`+taskID+`"}`)); !isErr || parse(t, text)["error"] != "UNKNOWN_TASK" {
		t.Errorf("consumed task should be unknown: %s", text)
	}
}

func mustTool(t *testing.T, s *Server, name, args string) string {
	t.Helper()
	text, _ := s.callTool(name, json.RawMessage(args))
	return text
}
