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
}
