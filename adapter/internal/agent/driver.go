// Package agent defines the AgentDriver interface and shared event types
// that let bbclaw-adapter multiplex multiple AI CLIs behind a single device
// protocol. See design/agent_bus.md in the public bbclaw repo for the
// architecture overview.
package agent

import (
	"context"
	"errors"
)

// SessionID identifies a running agent session inside the router.
type SessionID string

// ToolID identifies a single tool-use request that requires approval.
type ToolID string

// Decision is the user's response to a tool-use permission prompt.
type Decision string

const (
	DecisionOnce Decision = "once"
	DecisionDeny Decision = "deny"
)

// EventType enumerates the unified event stream shape. Individual drivers
// translate their CLI's native output into these.
type EventType string

const (
	EvText        EventType = "text"         // assistant text fragment
	EvToolCall    EventType = "tool_call"    // permission request (Capabilities.ToolApproval)
	EvStatus      EventType = "status"       // running/waiting/idle/offline
	EvTokens      EventType = "tokens"       // usage stats
	EvError       EventType = "error"        // driver-level error
	EvTurnEnd     EventType = "turn_end"     // one assistant turn finished
	EvSessionInit EventType = "session_init" // CLI reported its real session id (Text field)
)

// Event is the single type every driver emits on its Events channel.
type Event struct {
	Type EventType
	Seq  uint64

	Text string // text / status / error payload

	Tool   *ToolCall
	Tokens *Tokens
}

type ToolCall struct {
	ID   ToolID
	Tool string // "Bash" / "Edit" / ...
	Hint string // short preview suitable for a small display
}

type Tokens struct {
	In  int
	Out int
}

// Capabilities declares what a driver supports. Device-side UX adapts to
// this (e.g. hides the "approve" menu when ToolApproval is false).
type Capabilities struct {
	ToolApproval  bool `json:"toolApproval"`
	Resume        bool `json:"resume"`
	Streaming     bool `json:"streaming"`
	MaxInputBytes int  `json:"maxInputBytes"`
}

// StartOpts carries per-session startup parameters.
type StartOpts struct {
	ResumeID string            // non-empty => resume this CLI session
	Cwd      string            // working directory for the spawned process
	Env      map[string]string // extra env vars (merged onto os.Environ)
	// Model is the model id the driver should use for this session, picked
	// from one of the IDs the driver exposed via ModelLister.ListModels.
	// Empty means "driver's own default" (no --model arg / driver fallback).
	// Each driver decides how to honour it (claudecode/opencode/aider pass
	// it as --model, ollama uses it as the chat model field).
	Model string

	// SystemPrompt, when non-empty, is appended to the backend's system prompt
	// for this session. It carries the BBClaw "butler" device persona +
	// form-factor constraints (tiny screen / voice / PTT) assembled by the
	// butler layer (ADR-018); later it also carries user/project memory.
	// claudecode passes it as --append-system-prompt. Drivers that can't inject
	// a system prompt just ignore it (same contract as Model).
	SystemPrompt string
}

// Driver is the contract every per-CLI implementation must satisfy.
// See design/agent_bus.md §3 for lifecycle guarantees.
type Driver interface {
	Name() string
	Capabilities() Capabilities
	Start(ctx context.Context, opts StartOpts) (SessionID, error)
	Send(sid SessionID, text string) error
	Events(sid SessionID) <-chan Event
	Approve(sid SessionID, tid ToolID, decision Decision) error
	Stop(sid SessionID) error
}

// ErrUnsupported is returned by Approve on drivers where Capabilities.ToolApproval is false.
var ErrUnsupported = errors.New("agent: operation unsupported by this driver")

// ErrUnknownSession is returned when a SessionID does not exist in the driver.
var ErrUnknownSession = errors.New("agent: unknown session")

// SessionInfo describes a persisted CLI session that can be resumed.
type SessionInfo struct {
	ID           string `json:"id"`
	Preview      string `json:"preview"`
	LastUsed     int64  `json:"lastUsed"` // Unix seconds
	MessageCount int    `json:"messageCount"`
	Cwd          string `json:"cwd,omitempty"` // basename of the working directory; empty for drivers that don't track cwd
}

// SessionLister is an optional capability for drivers that can enumerate
// persisted sessions from disk. Drivers that don't implement this return
// an empty list from the HTTP endpoint (not an error).
type SessionLister interface {
	ListSessions(ctx context.Context, limit int) ([]SessionInfo, error)
}

// Message is one persisted turn from a session's conversation history.
// Returned by MessageLoader and surfaced to the device for transcript replay.
type Message struct {
	Role    string `json:"role"`    // "user" | "assistant"
	Content string `json:"content"` // plain text; multimodal content is flattened to text
	Seq     int    `json:"seq"`     // 0-based index into the underlying transcript (used as a pagination cursor)
}

// MessagesPage is the result of a paginated history load.
type MessagesPage struct {
	Messages []Message `json:"messages"` // chronological order (oldest first)
	Total    int       `json:"total"`    // total messages in the session (after filtering)
	HasMore  bool      `json:"hasMore"`  // true when earlier messages exist beyond the returned slice
}

// MessageLoader is an optional capability for drivers that can replay a
// session's history. Drivers that don't implement this cause the HTTP
// endpoint to return MESSAGES_NOT_SUPPORTED.
//
// before is the upper-exclusive seq cursor. before <= 0 means "the latest
// page" — return the last `limit` messages of the session. Otherwise return
// messages with seq < before, capped at `limit`.
type MessageLoader interface {
	LoadMessages(ctx context.Context, sid string, before, limit int) (MessagesPage, error)
}

// CLISessionChecker is an optional capability for drivers that store
// conversation history on disk. When implemented, the agent proxy uses it to
// validate a resume target before spawning a subprocess — if the on-disk
// transcript is missing the resume attempt is skipped entirely, avoiding the
// 4-7s cold-start penalty of a process that would immediately fail with
// SESSION_NOT_FOUND.
type CLISessionChecker interface {
	CLISessionExists(cliSessionID string) bool
}

// ModelInfo describes one model selectable under a driver. Surfaced to the
// device through GET /v1/agent/drivers so the settings UI can render a
// human-readable label while persisting the stable ID.
type ModelInfo struct {
	ID    string `json:"id"`              // stable id passed to the driver via StartOpts.Model
	Label string `json:"label,omitempty"` // short display label (defaults to ID when empty)
}

// ModelLister is an optional capability for drivers that can enumerate the
// set of models they support. Drivers that don't implement this surface an
// empty list (the device UI hides the Model row in that case).
//
// Implementations should be cheap and side-effect-free: the device may call
// this every time the settings screen is opened. Drivers that fetch the list
// from a remote source (e.g. ollama /api/tags) should impose their own short
// internal cache or short timeout — never block the request indefinitely.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ModelUpdater is an optional capability for drivers that allow the active
// model to be changed mid-session (between turns) — i.e. without recycling
// the underlying conversation. The HTTP / cloud layers call UpdateModel right
// before each Send so a user who toggles the model in the device Settings
// while inside a running session sees the new model on the *next* turn,
// instead of having to wait for the session to be evicted from the router's
// in-process cache.
//
// Implementations should be idempotent and cheap (just patch the session's
// model field under whatever lock the driver uses internally). Drivers that
// genuinely cannot switch models mid-conversation just don't implement this
// interface — StartOpts.Model still applies at session start.
type ModelUpdater interface {
	UpdateModel(sid SessionID, model string) error
}
