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
	EvText     EventType = "text"      // assistant text fragment
	EvToolCall EventType = "tool_call" // permission request (Capabilities.ToolApproval)
	EvStatus   EventType = "status"    // running/waiting/idle/offline
	EvTokens   EventType = "tokens"    // usage stats
	EvError    EventType = "error"     // driver-level error
	EvTurnEnd  EventType = "turn_end"  // one assistant turn finished
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
