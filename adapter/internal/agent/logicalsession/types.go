// Package logicalsession provides the device-facing session abstraction
// described by ADR-014. A LogicalSession outlives the underlying CLI
// conversation: when the CLI session id becomes invalid, the adapter mints
// a new CLI conversation and writes its id back into the LogicalSession,
// while the logical id stays stable. Devices only ever see the logical id.
package logicalsession

import "time"

// ID is a stable, BBClaw-minted identifier with "ls-" prefix to distinguish
// from CLI-native session ids (e.g., claude-code's "cc-..." or raw UUIDs).
type ID string

// Role distinguishes the session's place in the conversational-orchestrator
// hierarchy (ADR-021 §3): a device talks to a butler session, which dispatches
// work to N worker sessions. Worker sessions are internal dispatch artifacts
// and are never surfaced to the device (see Manager.ListDeviceFacing).
//
// The empty string is the legacy/default role: plain device-facing sessions
// minted before role-awareness existed deserialize as RoleNone and continue to
// behave exactly as before (backward compatible).
const (
	// RoleNone is the empty role: a plain, device-facing logical session.
	// This is the zero value, so existing on-disk records (which have no
	// "role" key) load as RoleNone and remain visible to devices.
	RoleNone = ""
	// RoleButler marks the per-device orchestrator session the device talks
	// to. It is device-facing (shown in menus/lists).
	RoleButler = "butler"
	// RoleWorker marks an internal session spawned by the butler via dispatch
	// (ADR-021 §2/§3). Workers are NOT shown to the device; they are reached
	// only through the butler's transcription.
	RoleWorker = "worker"
)

// LogicalSession is the device-facing session abstraction. It outlives the
// underlying CLI conversation: when the CLI session id becomes invalid, the
// adapter mints a new CLI session and writes its id back here while the
// logical id stays stable.
type LogicalSession struct {
	ID           ID        `json:"id"`
	DeviceID     string    `json:"deviceId"`
	Driver       string    `json:"driver"`
	Cwd          string    `json:"cwd"`
	CLISessionID string    `json:"cliSessionId,omitempty"` // empty until first turn
	Title        string    `json:"title,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	LastUsedAt   time.Time `json:"lastUsedAt"`
	// Role places the session in the butler/worker hierarchy (ADR-021 §3).
	// Empty (RoleNone) = legacy plain session. omitempty keeps existing
	// records byte-compatible: a session with no role serializes without the
	// key, and old records with no key load as RoleNone.
	Role string `json:"role,omitempty"`
	// CwdName is the human-readable CwdPool entry name for Cwd. It is
	// populated on outbound responses only (reverse-lookup against the
	// adapter's CwdPool config); it is never stored to disk.
	CwdName string `json:"cwdName,omitempty"`
}
