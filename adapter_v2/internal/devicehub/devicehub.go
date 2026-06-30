// Package devicehub holds a process-wide handle to the device's currently-active
// deviceapi.Bridge, so out-of-band producers (the reminder scheduler, ADR-042
// §3) can inject a turn into "the session the device is on" without owning the
// per-connection wiring.
//
// v2 runs a single shared default session (session.DefaultID) that the LAN device
// line and the cloud relay both drive, so ONE slot is enough: whichever transport
// most recently brought a live bridge online owns the slot. The slot is best-
// effort — Active() returns nil when no device line is up, and the scheduler then
// defers to the notify outbox (M3). It is NOT a registry of every bridge; it is
// the single "where is the device right now" pointer.
package devicehub

import (
	"sync"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
)

// Hub is a concurrency-safe single-slot holder of the active device bridge.
// The zero value is not usable; use New.
type Hub struct {
	mu     sync.RWMutex
	active *deviceapi.Bridge
}

// New builds an empty hub (no active bridge until a transport registers one).
func New() *Hub { return &Hub{} }

// Set records b as the active bridge. A transport calls this once its bridge is
// live (Run started). A later Set from another transport replaces it — last
// online wins, matching the single-device-line reality.
func (h *Hub) Set(b *deviceapi.Bridge) {
	h.mu.Lock()
	h.active = b
	h.mu.Unlock()
}

// Clear removes b from the slot IFF it is still the active one (compare-and-
// clear), so a stale teardown from an old connection doesn't wipe a newer
// bridge that already took the slot. A transport calls this on disconnect.
func (h *Hub) Clear(b *deviceapi.Bridge) {
	h.mu.Lock()
	if h.active == b {
		h.active = nil
	}
	h.mu.Unlock()
}

// Active returns the live bridge, or nil when no device line is up.
func (h *Hub) Active() *deviceapi.Bridge {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.active
}
