// serve_driver.go implements agent.Driver against a long-lived `opencode serve`
// over the official Go SDK (ADR-031). It is selected at construction time via
// NewServe (wired behind AGENT_OPENCODE_SERVE=1 in main.go) and coexists with
// the legacy CLI-scrape Driver in this package, so the migration is opt-in and
// reversible ("migrate, don't flip").
//
// Capabilities vs the legacy driver: native streaming text deltas, mid-session
// interrupt (abort), session enumeration, and provider-driven model listing —
// all from one stable, versioned API instead of scraping `opencode run` NDJSON.
//
// Event consumption is deliberately driven off the event TYPE STRING + raw
// `properties` JSON (see serve_events.go), NOT the SDK's typed event union: the
// installed server (1.15.1) emits `message.part.delta` which the SDK build
// (v0.19.2) does not model. Reading raw keeps us correct across that skew.
package opencode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// ServeDriver is the serve+SDK implementation of agent.Driver.
type ServeDriver struct {
	log   *obs.Logger
	serve *serveManager

	// toolApproval gates device-side tool approval (AGENT_OPENCODE_TOOL_APPROVAL).
	// When true: permission.asked surfaces as an approval EvToolCall the device
	// answers via Approve(); when false (default): permissions auto-approve so
	// device-less sessions never hang.
	toolApproval bool

	mu            sync.Mutex
	client        *opencode.Client
	baseURL       string
	routerStarted bool
	rootCtx       context.Context
	rootCancel    context.CancelFunc

	sessions map[agent.SessionID]*serveSession
	byOC     map[string]*serveSession // opencode ses_... → session

	// Butler dispatch (ADR-024/ADR-021-firmware-ui §1.2). MCP servers are
	// registered ONCE on the shared serve via POST /mcp (idempotent across butler
	// sessions). dispatchTools holds the tool names that signal a butler dispatch
	// (e.g. "bbclaw_dispatch") so the router maps them to EvDispatchStatus rather
	// than a plain EvToolCall.
	registeredMCP map[string]bool
	dispatchTools map[string]bool
}

// NewServe constructs the serve-backed driver. It does not spawn `opencode
// serve` yet — the server starts lazily on the first call that needs it, so an
// unused driver costs nothing.
func NewServe(opts Options, log *obs.Logger) *ServeDriver {
	d := &ServeDriver{
		log:           log,
		serve:         newServeManager(opts.Bin, log),
		toolApproval:  toolApprovalEnabled(),
		sessions:      make(map[agent.SessionID]*serveSession),
		byOC:          make(map[string]*serveSession),
		registeredMCP: make(map[string]bool),
		dispatchTools: make(map[string]bool),
	}
	d.rootCtx, d.rootCancel = context.WithCancel(context.Background())
	return d
}

func (d *ServeDriver) Name() string { return driverName }

func (d *ServeDriver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		// ToolApproval is opt-in (AGENT_OPENCODE_TOOL_APPROVAL): on → permission
		// requests are surfaced to the device; off → auto-approved in the router.
		ToolApproval:  d.toolApproval,
		Resume:        true,
		Streaming:     true,
		MaxInputBytes: 64 * 1024,
		Butler:        butlerEnabled(),
	}
}

// ensureReady starts the serve process (once), creates the SDK client, and
// launches the single shared event-router goroutine.
func (d *ServeDriver) ensureReady() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	base, err := d.serve.ensure(d.rootCtx)
	if err != nil {
		return err
	}
	if d.client == nil || d.baseURL != base {
		// No client-level timeout: it would apply to the long-lived `/event`
		// SSE stream and starve it. Per-turn timeout is set on the Prompt call.
		d.client = opencode.NewClient(option.WithBaseURL(base))
		d.baseURL = base
	}
	if !d.routerStarted {
		d.routerStarted = true
		go d.runEventRouter(d.rootCtx, d.client)
	}
	return nil
}

func (d *ServeDriver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	if err := d.ensureReady(); err != nil {
		return "", err
	}
	s := &serveSession{
		id:       agent.SessionID("oc-" + uuid.NewString()),
		events:   make(chan agent.Event, eventBufSize),
		cwd:      opts.Cwd,
		system:   strings.TrimSpace(opts.SystemPrompt),
		model:    strings.TrimSpace(opts.Model),
		toolSeen: make(map[string]bool),
		dispSeen: make(map[string]bool),
	}

	// Butler dispatch (ADR-024 §5): register the format-neutral MCP servers on
	// the shared serve. Non-fatal — a failure just drops dispatch this session.
	if len(opts.MCPServers) > 0 {
		if err := d.registerMCPServers(ctx, opts.MCPServers); err != nil {
			d.log.Warnf("opencode serve: register butler MCP failed: %v; session %s runs without dispatch", err, s.id)
		}
	}

	if opts.ResumeID != "" {
		// Resume: the opencode session already exists; reuse its id directly.
		s.ocID = opts.ResumeID
	} else {
		sess, err := d.client.Session.New(ctx, opencode.SessionNewParams{})
		if err != nil {
			return "", fmt.Errorf("opencode serve: create session: %w", err)
		}
		s.ocID = sess.ID
	}

	d.mu.Lock()
	d.sessions[s.id] = s
	d.byOC[s.ocID] = s
	d.mu.Unlock()
	d.log.Infof("opencode serve: session start id=%s oc=%s resume=%t model=%q", s.id, s.ocID, opts.ResumeID != "", s.model)
	return s.id, nil
}

func (d *ServeDriver) Events(sid agent.SessionID) <-chan agent.Event {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		ch := make(chan agent.Event)
		close(ch)
		return ch
	}
	return s.events
}

// Send issues one turn. It BLOCKS until the turn completes (matching the legacy
// driver's contract: the caller runs Send in a goroutine while draining
// Events). Streaming text/thinking/tool/token events arrive on the session
// channel from the shared router during the call; Send emits the terminal
// EvTurnEnd (and EvInterrupted, if aborted) once Prompt returns.
func (d *ServeDriver) Send(sid agent.SessionID, text string) (sendErr error) {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	client := d.client
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

	defer func() {
		if sendErr != nil {
			s.emit(agent.Event{Type: agent.EvError, Text: sendErr.Error()})
		}
		if s.consumeInterrupted() {
			s.emit(agent.Event{Type: agent.EvInterrupted})
		}
		s.emit(agent.Event{Type: agent.EvTurnEnd})
	}()

	params := opencode.SessionPromptParams{
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Type: opencode.F(opencode.TextPartInputTypeText),
				Text: opencode.F(text),
			},
		}),
	}
	if s.cwd != "" {
		params.Directory = opencode.F(s.cwd)
	}
	if s.system != "" {
		params.System = opencode.F(s.system)
	}
	if provID, modID, ok := splitModel(s.model); ok {
		params.Model = opencode.F(opencode.SessionPromptParamsModel{
			ProviderID: opencode.F(provID),
			ModelID:    opencode.F(modID),
		})
	}

	d.log.Infof("opencode serve: send oc=%s text=%q", s.ocID, truncate(text, 200))
	if _, err := client.Session.Prompt(d.rootCtx, s.ocID, params, option.WithRequestTimeout(15*time.Minute)); err != nil {
		if s.wasInterrupted() {
			// Abort surfaces as a prompt error; that's expected, not a failure.
			return nil
		}
		return fmt.Errorf("opencode serve: prompt: %w", err)
	}
	return nil
}

// Approve answers a tool-approval prompt (tid == the opencode permissionID
// surfaced as the EvToolCall ID). Only meaningful when ToolApproval is on;
// otherwise permissions are auto-approved in the router and this returns
// ErrUnsupported.
func (d *ServeDriver) Approve(sid agent.SessionID, tid agent.ToolID, dec agent.Decision) error {
	if !d.toolApproval {
		return agent.ErrUnsupported
	}
	d.mu.Lock()
	s, ok := d.sessions[sid]
	client := d.client
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	resp := opencode.SessionPermissionRespondParamsResponseOnce
	if dec == agent.DecisionDeny {
		resp = opencode.SessionPermissionRespondParamsResponseReject
	}
	_, err := client.Session.Permissions.Respond(d.rootCtx, s.ocID, string(tid),
		opencode.SessionPermissionRespondParams{Response: opencode.F(resp)})
	if err != nil {
		return fmt.Errorf("opencode serve: respond permission %s: %w", tid, err)
	}
	return nil
}

// toolApprovalEnabled reports whether device tool-approval is opted in.
func toolApprovalEnabled() bool {
	v := strings.TrimSpace(os.Getenv("AGENT_OPENCODE_TOOL_APPROVAL"))
	return v == "1" || strings.EqualFold(v, "true")
}

// UpdateModel implements agent.ModelUpdater — applies on the next turn.
func (d *ServeDriver) UpdateModel(sid agent.SessionID, model string) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.mu.Lock()
	s.model = strings.TrimSpace(model)
	s.mu.Unlock()
	return nil
}

// Interrupt aborts the in-flight turn without destroying the session
// (barge-in, ADR-028 §2.5.1). The session id survives for the next Send.
func (d *ServeDriver) Interrupt(sid agent.SessionID) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	client := d.client
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.markInterrupted()
	if client != nil {
		_, _ = client.Session.Abort(d.rootCtx, s.ocID, opencode.SessionAbortParams{})
	}
	return nil
}

func (d *ServeDriver) Stop(sid agent.SessionID) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	client := d.client
	if ok {
		delete(d.sessions, sid)
		delete(d.byOC, s.ocID)
	}
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	if client != nil {
		_, _ = client.Session.Abort(d.rootCtx, s.ocID, opencode.SessionAbortParams{})
	}
	close(s.events)
	return nil
}

// Shutdown tears down the shared serve process and router. Not part of
// agent.Driver; called by main on adapter shutdown if wired.
func (d *ServeDriver) Shutdown() {
	if d.rootCancel != nil {
		d.rootCancel()
	}
	d.serve.stop()
}

// ── session ─────────────────────────────────────────────────────────────────

type serveSession struct {
	id     agent.SessionID
	ocID   string
	events chan agent.Event
	cwd    string
	system string

	seq atomic.Uint64

	mu          sync.Mutex
	model       string
	interrupted bool
	// per-callID dedup for tool part updates (each tool fires pending→running→
	// completed): toolSeen gates the single display EvToolCall; dispSeen gates the
	// single EvDispatchStatus "started".
	toolSeen map[string]bool
	dispSeen map[string]bool
}

func (s *serveSession) emit(e agent.Event) {
	e.Seq = s.seq.Add(1)
	select {
	case s.events <- e:
	default:
		// Drop on a full buffer rather than blocking the shared router.
	}
}

// markToolSeen returns true the first time a tool callID is seen (display dedup).
func (s *serveSession) markToolSeen(callID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolSeen[callID] {
		return false
	}
	s.toolSeen[callID] = true
	return true
}

// markDispStarted returns true the first time a dispatch callID is seen
// (so the "started" phase is emitted once).
func (s *serveSession) markDispStarted(callID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dispSeen[callID] {
		return false
	}
	s.dispSeen[callID] = true
	return true
}

func (s *serveSession) markInterrupted()     { s.mu.Lock(); s.interrupted = true; s.mu.Unlock() }
func (s *serveSession) wasInterrupted() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.interrupted }
func (s *serveSession) consumeInterrupted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.interrupted
	s.interrupted = false
	return v
}

// splitModel turns "provider/model-id" into its parts. Returns ok=false when
// the model string is empty or not in provider/model form (driver default).
func splitModel(m string) (provider, model string, ok bool) {
	m = strings.TrimSpace(m)
	if m == "" {
		return "", "", false
	}
	if p, id, found := strings.Cut(m, "/"); found && p != "" && id != "" {
		return p, id, true
	}
	return "", "", false
}

// compile-time interface assertions.
var (
	_ agent.Driver       = (*ServeDriver)(nil)
	_ agent.Interrupter  = (*ServeDriver)(nil)
	_ agent.ModelUpdater = (*ServeDriver)(nil)
)
