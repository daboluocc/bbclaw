// Package butlermcp is the stdio MCP server the conversational orchestrator
// butler (ADR-021) talks to. The butler runs as `claude -p --resume
// --mcp-config <cfg>`; this server is the stdio subprocess that --mcp-config
// points at. It exposes the dispatch tools the butler uses to spawn worker
// claude-code sessions in target project directories:
//
//	list_projects()                       -> the allow-listed projects (CwdPool)
//	dispatch(project|cwd, task, wait?)    -> run a worker; inline result if it
//	                                         finishes within `wait`, else async
//	task_status(taskId) / task_result(id) -> poll a degraded-to-async task
//
// The MCP wire protocol verified against claude 2.1.158 (ADR-021 §前置闸门):
// newline-delimited JSON-RPC 2.0 over stdio. stdout carries ONLY JSON-RPC — all
// logging goes to stderr.
package butlermcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)


// protocolVersion is the MCP protocol revision we advertise; echoed from the
// client's initialize when present.
const defaultProtocolVersion = "2024-11-05"

// defaultDispatchWait is how long dispatch blocks for a worker before degrading
// to async (ADR-021 §2: short tasks inline, long tasks async).
const defaultDispatchWait = 25 * time.Second

// workerMaxRuntime caps a worker's total runtime even after it has degraded to
// async, so a hung worker can't live forever.
const workerMaxRuntime = 15 * time.Minute

// Project is one allow-listed working directory the butler may dispatch into.
type Project struct {
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

// WorkerRunner runs one agentic task in cwd and returns the worker's final
// result text. Production wraps the claudecode driver (runner_claude.go); tests
// inject a mock. Run must honour ctx cancellation.
type WorkerRunner interface {
	Run(ctx context.Context, cwd, task string) (string, error)
}

// Logger is the minimal logging surface (to stderr — never stdout).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...any) {}
func (nopLogger) Warnf(string, ...any) {}

// Server is the stdio MCP server. Construct with New, then Serve(os.Stdin, os.Stdout).
type Server struct {
	projects   []Project        // static fallback when projectsFn is nil
	projectsFn func() []Project // live allow-list source (re-read per tool call)
	runner     WorkerRunner
	wait       time.Duration
	log        Logger
	bg         context.Context // background ctx for async workers (survives the dispatch call)
	newID      func() string
	memWriter  MemoryWriter // optional; nil disables the remember tool
	store      *TaskStore   // persistent task state (survives mcp-server restarts)
}

// Options configure a Server.
type Options struct {
	Projects []Project
	// ProjectsProvider, when non-nil, is consulted on every list_projects /
	// dispatch instead of the static Projects slice. It lets the allow-list stay
	// live: an admin add in the main process is picked up here (a separate
	// subprocess) without a restart. Falls back to Projects when nil.
	ProjectsProvider func() []Project
	Runner           WorkerRunner
	DispatchWait     time.Duration // 0 => defaultDispatchWait
	Log              Logger        // nil => no-op
	// BackgroundCtx is the parent ctx for async workers. nil => context.Background().
	BackgroundCtx context.Context
	// MemoryWriter, when non-nil, enables the `remember` MCP tool so the butler
	// can write directly into MEMORY/*.md files (Layer 1 of the long-term memory
	// pipeline, #124). nil disables the tool entirely (safe default).
	MemoryWriter MemoryWriter
	// DataDir, when non-empty, enables persistent task state under
	// <DataDir>/task-runs/. Tasks survive mcp-server restarts (fix #162).
	// Empty string disables persistence (memory-only; used in tests).
	DataDir string
}

// New builds a Server.
func New(opts Options) *Server {
	wait := opts.DispatchWait
	if wait <= 0 {
		wait = defaultDispatchWait
	}
	log := opts.Log
	if log == nil {
		log = nopLogger{}
	}
	bg := opts.BackgroundCtx
	if bg == nil {
		bg = context.Background()
	}
	return &Server{
		projects:   opts.Projects,
		projectsFn: opts.ProjectsProvider,
		runner:     opts.Runner,
		wait:       wait,
		log:        log,
		bg:         bg,
		newID:      newTaskID,
		store:      NewTaskStore(opts.DataDir),
		memWriter:  opts.MemoryWriter,
	}
}

// ─────────────────────────── JSON-RPC plumbing ───────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the read/dispatch loop until in is exhausted (client closed stdin).
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // tool args can be large
	var wmu sync.Mutex
	enc := json.NewEncoder(out)
	write := func(resp rpcResponse) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = enc.Encode(resp) // newline-delimited JSON
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.log.Warnf("butlermcp: bad json-rpc line: %v", err)
			continue
		}
		s.handle(req, write)
	}
	return sc.Err()
}

func (s *Server) reply(write func(rpcResponse), id json.RawMessage, result any) {
	if id == nil {
		return // notification — no response
	}
	write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyErr(write func(rpcResponse), id json.RawMessage, code int, msg string) {
	if id == nil {
		return
	}
	write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) handle(req rpcRequest, write func(rpcResponse)) {
	switch req.Method {
	case "initialize":
		pv := defaultProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			pv = p.ProtocolVersion
		}
		s.reply(write, req.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "bbclaw-butler", "version": "0.1.0"},
		})
	case "notifications/initialized", "notifications/cancelled":
		// notifications: no response
	case "tools/list":
		s.reply(write, req.ID, map[string]any{"tools": toolDefs()})
	case "tools/call":
		s.handleToolCall(req, write)
	default:
		// Any other request gets an empty result so the client never hangs;
		// notifications (no id) are silently ignored.
		s.reply(write, req.ID, map[string]any{})
	}
}

// ─────────────────────────── tools ───────────────────────────

func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "list_projects",
			"description": "List the project directories you are allowed to dispatch tasks into. Returns name + cwd for each.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
		},
		{
			"name":        "dispatch",
			"description": "Run a coding task by spawning a worker agent in a project directory. Identify the project by `project` (name from list_projects) or `cwd` (must be allow-listed). Returns {status:\"done\", result} if it finishes within wait_seconds, else {status:\"running\", taskId} — poll with task_status/task_result.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project":      map[string]any{"type": "string", "description": "project name from list_projects"},
					"cwd":          map[string]any{"type": "string", "description": "absolute project path (must be allow-listed)"},
					"task":         map[string]any{"type": "string", "description": "the task for the worker agent to perform"},
					"wait_seconds": map[string]any{"type": "number", "description": "max seconds to wait inline before degrading to async (default 25)"},
				},
				"required": []string{"task"},
			},
		},
		{
			"name":        "task_status",
			"description": "Check the status of an async task returned by dispatch. Returns {status: running|done|error}.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"taskId": map[string]any{"type": "string"}}, "required": []string{"taskId"}},
		},
		{
			"name":        "task_result",
			"description": "Get the result of a finished async task. Returns {result} or an error if still running.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"taskId": map[string]any{"type": "string"}}, "required": []string{"taskId"}},
		},
		{
			"name":        "remember",
			"description": "Persist a memory note into the butler's long-term memory. Use this when the user shares stable identity information, preferences, project context, or key decisions that should survive across sessions. Writes into MEMORY/<category>.md inside the butler's managed block, preserving any existing content outside it.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"category": map[string]any{
						"type":        "string",
						"enum":        []string{"profile", "preferences", "projects", "decisions"},
						"description": "Memory bucket: profile=user identity/onboarding, preferences=stable habits/tastes, projects=recent work context, decisions=key choices made",
					},
					"text": map[string]any{
						"type":        "string",
						"description": "One-sentence fact or note to persist (speakable, in the user's language). Must not contain instruction-like content.",
					},
				},
				"required": []string{"category", "text"},
			},
		},
	}
}

func (s *Server) handleToolCall(req rpcRequest, write func(rpcResponse)) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil {
		s.replyErr(write, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	text, isErr := s.callTool(call.Name, call.Arguments)
	s.reply(write, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	})
}

// callTool runs one tool and returns (text, isError). text is a JSON string the
// butler reads back. Errors are returned as tool-result text with isError=true
// (not JSON-RPC errors) so the butler sees the message and can react.
func (s *Server) callTool(name string, args json.RawMessage) (string, bool) {
	switch name {
	case "list_projects":
		return jsonStr(map[string]any{"projects": s.currentProjects()}), false
	case "dispatch":
		return s.toolDispatch(args)
	case "task_status":
		return s.toolTaskStatus(args)
	case "task_result":
		return s.toolTaskResult(args)
	case "remember":
		return s.toolRemember(args)
	default:
		return jsonStr(map[string]any{"error": "UNKNOWN_TOOL", "detail": name}), true
	}
}

func (s *Server) toolDispatch(args json.RawMessage) (string, bool) {
	var a struct {
		Project     string  `json:"project"`
		Cwd         string  `json:"cwd"`
		Task        string  `json:"task"`
		WaitSeconds float64 `json:"wait_seconds"`
	}
	_ = json.Unmarshal(args, &a)
	task := strings.TrimSpace(a.Task)
	if task == "" {
		return jsonStr(map[string]any{"error": "EMPTY_TASK"}), true
	}
	cwd, ok := s.resolveCwd(a.Project, a.Cwd)
	if !ok {
		return jsonStr(map[string]any{
			"error":  "PROJECT_NOT_ALLOWED",
			"detail": "use a project name from list_projects, or an allow-listed cwd",
		}), true
	}

	wait := s.wait
	if a.WaitSeconds > 0 {
		wait = time.Duration(a.WaitSeconds * float64(time.Second))
	}

	id := s.newID()
	if err := s.store.Create(id, cwd, task); err != nil {
		s.log.Warnf("butlermcp: store.Create failed task=%s: %v", id, err)
	}

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(s.bg, workerMaxRuntime)
		defer cancel()
		res, err := s.runner.Run(ctx, cwd, task)
		if err != nil {
			if werr := s.store.MarkError(id, err.Error()); werr != nil {
				s.log.Warnf("butlermcp: store.MarkError task=%s: %v", id, werr)
			}
		} else {
			if werr := s.store.MarkDone(id, res); werr != nil {
				s.log.Warnf("butlermcp: store.MarkDone task=%s: %v", id, werr)
			}
		}
		close(done)
	}()

	select {
	case <-done:
		run, found := s.store.Get(id)
		if !found {
			return jsonStr(map[string]any{"error": "INTERNAL", "detail": "task vanished after completion"}), true
		}
		if run.Status == TaskStatusError {
			return jsonStr(map[string]any{"status": "error", "taskId": id, "error": run.Error}), true
		}
		return jsonStr(map[string]any{"status": "done", "result": run.Result}), false
	case <-time.After(wait):
		// degraded to async — worker keeps running; butler polls task_status.
		s.log.Infof("butlermcp: dispatch degraded to async task=%s cwd=%q", id, cwd)
		return jsonStr(map[string]any{"status": "running", "taskId": id}), false
	}
}

func (s *Server) toolTaskStatus(args json.RawMessage) (string, bool) {
	id := taskIDArg(args)
	run, ok := s.store.Get(id)
	if !ok {
		return jsonStr(map[string]any{"error": "UNKNOWN_TASK", "detail": id}), true
	}
	return jsonStr(map[string]any{"status": run.Status}), false
}

func (s *Server) toolTaskResult(args json.RawMessage) (string, bool) {
	id := taskIDArg(args)
	run, ok := s.store.Get(id)
	if !ok {
		return jsonStr(map[string]any{"error": "UNKNOWN_TASK", "detail": id}), true
	}
	switch run.Status {
	case TaskStatusRunning:
		return jsonStr(map[string]any{"status": "running"}), false
	case TaskStatusError:
		return jsonStr(map[string]any{"status": "error", "error": run.Error}), true
	default:
		return jsonStr(map[string]any{"status": "done", "result": run.Result}), false
	}
}

// resolveCwd maps a (project|cwd) selection to an allow-listed cwd. A project
// name must match a configured Project; a raw cwd must equal an allow-listed
// Cwd. Anything else is rejected (ADR-021 §2 security: no arbitrary cwd).
func (s *Server) resolveCwd(project, cwd string) (string, bool) {
	project = strings.TrimSpace(project)
	cwd = strings.TrimSpace(cwd)
	projects := s.currentProjects()
	if project != "" {
		for _, p := range projects {
			if p.Name == project {
				return p.Cwd, true
			}
		}
		return "", false
	}
	if cwd != "" {
		for _, p := range projects {
			if p.Cwd == cwd {
				return p.Cwd, true
			}
		}
		return "", false
	}
	// Neither given: fall back to the single configured project if there's
	// exactly one (common single-project setup); otherwise reject.
	if len(projects) == 1 {
		return projects[0].Cwd, true
	}
	return "", false
}

// currentProjects returns the live allow-list: the provider's result when one is
// wired (so runtime admin adds are honoured without a restart), else the static
// snapshot captured at construction.
func (s *Server) currentProjects() []Project {
	if s.projectsFn != nil {
		return s.projectsFn()
	}
	return s.projects
}

// ─────────────────────────── helpers ───────────────────────────

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":"MARSHAL_FAILED","detail":%q}`, err.Error())
	}
	return string(b)
}

func taskIDArg(args json.RawMessage) string {
	var a struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(args, &a)
	return strings.TrimSpace(a.TaskID)
}

func newTaskID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "task-" + fmt.Sprint(time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(buf[:])
}

// toolRemember handles the `remember` MCP tool. It writes text into the butler's
// long-term MEMORY/<category>.md file via the injected MemoryWriter. When no
// MemoryWriter is configured the tool returns a clear error so the butler knows
// the feature is not available rather than silently discarding the note.
func (s *Server) toolRemember(args json.RawMessage) (string, bool) {
	if s.memWriter == nil {
		return jsonStr(map[string]any{
			"error":  "REMEMBER_UNAVAILABLE",
			"detail": "memory writer not configured; set BBCLAW_BUTLER_MEMORY_DISTILL=1 or wire MemoryWriter in Options",
		}), true
	}
	var a struct {
		Category string `json:"category"`
		Text     string `json:"text"`
	}
	_ = json.Unmarshal(args, &a)
	a.Category = strings.TrimSpace(a.Category)
	a.Text = strings.TrimSpace(a.Text)
	if a.Category == "" || a.Text == "" {
		return jsonStr(map[string]any{"error": "INVALID_ARGS", "detail": "category and text are required"}), true
	}
	if _, ok := validCategories[a.Category]; !ok {
		return jsonStr(map[string]any{
			"error":  "UNKNOWN_CATEGORY",
			"detail": "category must be one of: profile, preferences, projects, decisions",
		}), true
	}
	if err := s.memWriter.WriteMemory(a.Category, a.Text); err != nil {
		s.log.Warnf("butlermcp: remember failed category=%q: %v", a.Category, err)
		return jsonStr(map[string]any{"error": "WRITE_FAILED", "detail": err.Error()}), true
	}
	s.log.Infof("butlermcp: remember ok category=%q", a.Category)
	return jsonStr(map[string]any{"ok": true, "category": a.Category}), false
}
