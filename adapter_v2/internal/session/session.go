// Package session owns the lifecycle of a PTY-backed agent session and keeps it
// alive independently of any client connection. This is the core of v2's
// "refresh = resume" behaviour, modelled on dinotty src/session.rs:
//
//   - the PTY child keeps running after all clients disconnect (Detached);
//   - a reconnecting client Attaches and is sent a screen Snapshot;
//   - a GC reaps sessions Detached longer than detachTimeout.
//
// One Session feeds TWO consumers off the SAME byte stream:
//   - raw bytes → terminal clients (package termchan)
//   - vtscreen  → extracted text → device/voice (package extract + deviceapi)
package session

import (
	"errors"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/vtscreen"
)

// ErrClosed is returned by Write when the session's PTY has already exited (or
// was never spawned), so the caller can distinguish a dead session from a
// transient write error.
var ErrClosed = errors.New("session: pty closed")

// DefaultID is the well-known id of the "default active" session that the device
// takes over and the web terminal joins by default (no ?session=). The device
// (LAN or cloud relay) and the web client resolve to this same id so they attach
// to ONE shared PTY — the device extracts voice off it while the web client views
// the raw terminal. (P1 of the unified session model: single default session;
// per-logical-session ids and a web session-browser come later.)
const DefaultID = "default"

// DefaultGridCols/Rows are the FIXED grid the default (device) session's PTY is
// spawned at and pinned to for its whole life — it is never resized at runtime.
//
// Why fixed-and-generous: the device line screen-scrapes claude's TUI off this
// grid (internal/extract). A reply taller than the visible grid scrolls its "⏺"
// anchor off the top and the scrape loses it — the long-list-reply TTS-truncation
// bug (extract/CASES.md C9, ADR-035). A tall grid keeps realistic replies on one
// screen so extraction rarely has to lean on scrollback recovery. The web
// terminal that joins this session is a FIXED-SIZE VIEWER (it frames this grid
// with CSS, see web/spa TerminalView.vue) and deliberately does NOT drive the PTY
// size — so a small browser viewport can never starve the device's extraction.
const (
	DefaultGridCols = 120
	DefaultGridRows = 60
)

// detachTimeout is how long a session may sit with no clients before the GC
// reaps it (and kills the child). Mirrors dinotty's 300s cleanup window.
const detachTimeout = 5 * time.Minute

// gcInterval is how often the reaper scans for expired detached sessions.
const gcInterval = 30 * time.Second

// clientBuf is the per-client output buffer depth. A client that falls this far
// behind is dropped on the next chunk (its Out send hits the default case); it
// resyncs from scratch via Snapshot on the next Attach. Decoupling per-client
// buffering from the broadcast lock is what lets pumpOutput broadcast without
// ever blocking on a slow socket.
const clientBuf = 256

// Status tracks whether any client is currently attached.
type Status int

const (
	Connected Status = iota
	Detached
)

// Client is a subscriber to the raw PTY output stream (one terminal client, or
// the device extraction loop). The Session pushes byte chunks; a slow/closed
// client is dropped. Out is closed by the Session when the PTY exits so readers
// can observe end-of-stream.
type Client struct {
	Out chan []byte
}

// Session wraps one live PTY + its mirrored screen, shared by all consumers.
type Session struct {
	ID string

	mu       sync.Mutex
	pty      ptyhost.PTY
	screen   *vtscreen.Screen
	clients  []*Client
	status   Status
	detached time.Time // when status flipped to Detached (zero while Connected)
	exited   bool      // pumpOutput has seen the PTY hit EOF; child is gone

	// onExit, if set, is invoked once when the PTY exits so the Manager can
	// drop the session from its map. Called without the session lock held.
	onExit func()
}

// pumpOutput reads the PTY forever, feeding the screen and broadcasting raw
// bytes to every attached client. Mirrors dinotty pty.rs's reader loop.
//
// On PTY exit it marks the session exited, closes every client channel (so
// terminal/extract readers observe end-of-stream) and fires onExit so the
// Manager evicts the session.
func (s *Session) pumpOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			// Copy out of the shared read buffer: the chunk is handed to the
			// screen and fanned out to client channels that outlive this loop.
			chunk := append([]byte(nil), buf[:n]...)
			s.broadcast(chunk)
		}
		if err != nil {
			s.finish()
			return
		}
	}
}

// broadcast feeds one chunk into the screen and fans it out to every attached
// client. The screen Feed and the client list are both guarded by s.mu, but the
// fan-out never blocks: each client has a buffered channel and a full channel
// drops the chunk (the client resyncs via Snapshot on its next Attach). This is
// the "do not hold the lock across a blocking send" rule — buffered + drop.
func (s *Session) broadcast(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.screen.Feed(chunk)
	for _, c := range s.clients {
		select {
		case c.Out <- chunk:
		default: // drop on backpressure; client resyncs via Snapshot
		}
	}
}

// finish runs once when the PTY exits (self-exited child): it closes all client
// channels, releases the PTY master fd, reaps the child, marks the session
// exited, and notifies the Manager. Idempotent guard via s.exited.
//
// Unlike the GC path (kill → reap), a self-exited child is NOT reachable via the
// Manager map by the time the reaper runs (finish's onExit already evicted it),
// so finish must release the PTY itself or the master fd and zombie would leak.
func (s *Session) finish() {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	pty := s.pty
	for _, c := range s.clients {
		close(c.Out)
	}
	s.clients = nil
	onExit := s.onExit
	s.mu.Unlock()

	// Evict from the Manager map FIRST so the session is promptly unreachable
	// (this is the contract observers rely on: once the child exits, Get returns
	// nil). Eviction must not be delayed behind the blocking Wait below. The
	// session is already marked exited, so the GC reaper can never double-reap it.
	if onExit != nil {
		onExit()
	}

	// Then release the master fd and reap the child. pumpOutput has already
	// observed the read error (EOF), so the child is gone; Close best-effort kills
	// it and Wait collects the exit status so it does not linger as a zombie.
	if pty != nil {
		_ = pty.Close()
		_, _ = pty.Wait()
	}
}

// Attach registers a new raw-byte client and returns the current screen
// snapshot (scrollback chunks + visible grid) so the caller can replay it
// before forwarding live output. Registration and snapshot happen atomically
// under the lock so a chunk can never land in both the snapshot and the live
// stream (or in neither). Sets status back to Connected.
//
// If the PTY has already exited the returned Client.Out is a pre-closed channel
// and the snapshot still reflects the final screen, so a late joiner sees the
// last state and an immediate end-of-stream rather than hanging.
func (s *Session) Attach() (*Client, [][]byte, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Client{Out: make(chan []byte, clientBuf)}
	// Snapshot under the same lock that guards Feed in broadcast, so the
	// scrollback + grid we hand back are exactly the bytes already consumed and
	// every later chunk goes to the live channel — no gap, no double-delivery.
	scrollback := s.screen.ScrollbackChunks(200)
	snapshot := s.screen.Snapshot()
	if s.exited {
		// PTY already gone: nothing will ever write to this client. Hand back a
		// closed channel so the reader unblocks immediately.
		close(c.Out)
		return c, scrollback, snapshot
	}
	s.clients = append(s.clients, c)
	s.status = Connected
	s.detached = time.Time{}
	return c, scrollback, snapshot
}

// Detach removes a client (its socket closed) and, if it was the last one,
// flips the session to Detached so the GC clock starts ticking. Idempotent:
// detaching an unknown/already-removed client is a no-op.
func (s *Session) Detach(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.clients {
		if existing == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			break
		}
	}
	if len(s.clients) == 0 && !s.exited {
		s.status = Detached
		s.detached = time.Now()
	}
}

// Status reports whether any client is currently attached.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// VisibleText returns the current visible-screen text (no ANSI), read off the
// mirrored screen under the session lock. Callers use it to detect readiness —
// e.g. the web composer waits for claude's idle "❯" prompt before injecting a
// freshly-spawned session's first turn, the same heuristic deviceapi warmup uses.
func (s *Session) VisibleText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screen.VisibleText()
}

// Size reports the session's current grid size (cols, rows), read off the
// mirrored screen under the session lock. termchan sends this in the
// reconnected message so a joining terminal client sizes its xterm.js to match
// the live screen before replaying the snapshot.
func (s *Session) Size() ptyhost.Size {
	s.mu.Lock()
	defer s.mu.Unlock()
	cols, rows := s.screen.Size()
	return ptyhost.Size{Cols: uint16(cols), Rows: uint16(rows)}
}

// Resize forwards a new grid size to both the PTY (so the child reflows) and the
// mirrored screen (so snapshots match). Safe to call before the child has read.
func (s *Session) Resize(sz ptyhost.Size) error {
	s.mu.Lock()
	pty := s.pty
	s.screen.Resize(int(sz.Cols), int(sz.Rows))
	s.mu.Unlock()
	if pty == nil {
		return nil
	}
	return pty.Resize(sz)
}

// Write forwards input bytes (keystrokes or injected ASR text) to the PTY
// stdin. Input multiplexing policy (who may write) is enforced by the caller.
func (s *Session) Write(p []byte) error {
	s.mu.Lock()
	pty := s.pty
	exited := s.exited
	s.mu.Unlock()
	if pty == nil || exited {
		return ErrClosed
	}
	_, err := pty.Write(p)
	return err
}

// kill tears down the PTY and reaps the child. Used by the GC when reaping an
// expired detached session. Safe to call more than once, and safe to race with
// finish(): the s.exited guard ensures the PTY is Closed+Waited exactly once,
// so a self-exited child whose finish() is still in flight is not torn down
// twice (a double Close/Wait on creack/pty is harmless but we avoid it anyway).
func (s *Session) kill() {
	s.mu.Lock()
	if s.exited {
		// finish() already (or concurrently) owns teardown of this PTY.
		s.mu.Unlock()
		return
	}
	s.exited = true
	pty := s.pty
	// Close the client channels so any reader observes end-of-stream, matching
	// finish()'s contract for a reaped session.
	for _, c := range s.clients {
		close(c.Out)
	}
	s.clients = nil
	s.mu.Unlock()

	if pty == nil {
		return
	}
	_ = pty.Close()
	// Reap the child so it does not linger as a zombie. Close already best-effort
	// kills it; Wait just collects the exit status.
	_, _ = pty.Wait()
}

// Manager holds all live sessions and reaps detached ones.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	// creating serializes concurrent GetOrCreate calls for the SAME id so a PTY
	// is spawned at most once per id even under a create-if-absent race. It is a
	// separate lock from mu so we never spawn a PTY while holding the map lock.
	creating map[string]*sync.Mutex

	// detachTimeout/gcInterval are fields (defaulted in NewManager) so tests can
	// drive the reaper on a short clock.
	detachTimeout time.Duration
	gcInterval    time.Duration
}

// NewManager returns an empty session manager with the default detach/GC
// timings and starts its reaper goroutine.
func NewManager() *Manager {
	m := &Manager{
		sessions:      map[string]*Session{},
		creating:      map[string]*sync.Mutex{},
		detachTimeout: detachTimeout,
		gcInterval:    gcInterval,
	}
	go m.gcLoop()
	return m
}

// Get returns the session by id, or nil.
func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Remove kills the session for id (terminating its CLI child) and drops it from
// the map, so the next GetOrCreate spawns a fresh one. Used to respawn the shared
// default session when switching conversations (new / resume a different id):
// killing it closes the client channels, so an attached Bridge's Run returns
// ErrClosed and a web terminal sees a clean disconnect + reconnect. No-op if the
// id isn't live.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	if s != nil {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if s != nil {
		s.kill()
	}
}

// GetOrCreate returns the live session for id, spawning one with cfg if absent.
// It is the atomic create-if-absent the /ws handler needs: two near-simultaneous
// first connections to the same id (a phone and a web page opening the same
// shareable link at once, or a reconnect racing a second tab) must end up sharing
// ONE PTY child, not racing two Get-then-Create calls that each spawn a process
// and leak the loser.
//
// Concurrency: a per-id creation mutex serializes callers for the same id so the
// PTY is spawned at most once, while the spawn happens OUTSIDE m.mu (spawning is
// slow and must not block the whole map). Callers for different ids never block
// each other. If a session already exists (or appeared while we waited on the
// per-id lock) we return it without spawning.
func (m *Manager) GetOrCreate(id string, cfg ptyhost.Config) (*Session, error) {
	// Fast path: already live.
	if s := m.Get(id); s != nil {
		return s, nil
	}

	// Serialize creation for this id. Only one caller spawns; the rest wait here
	// and then observe the winner's session via the re-check below.
	lock := m.creationLock(id)
	lock.Lock()
	defer m.releaseCreationLock(id, lock)

	// Re-check under the per-id lock: a concurrent caller may have created it
	// while we waited (or it may have been created and not yet reaped).
	if s := m.Get(id); s != nil {
		return s, nil
	}
	return m.Create(id, cfg)
}

// creationLock returns the per-id creation mutex, allocating it on first use.
func (m *Manager) creationLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.creating[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.creating[id] = lock
	}
	return lock
}

// releaseCreationLock unlocks the per-id creation mutex and drops it from the map
// if no other caller is currently holding/awaiting it, so the map does not grow
// unbounded across distinct session ids. We delete only when the stored lock is
// still the one we used AND a non-blocking TryLock succeeds (meaning nobody else
// is waiting on it right now).
func (m *Manager) releaseCreationLock(id string, lock *sync.Mutex) {
	lock.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.creating[id] != lock {
		return
	}
	// If another goroutine is blocked on lock.Lock(), TryLock fails and we leave
	// the entry in place for them to reuse. Otherwise reclaim it.
	if lock.TryLock() {
		lock.Unlock()
		delete(m.creating, id)
	}
}

// Create spawns a new PTY session under the given id, wires its screen to the
// requested grid size, and starts pumping output. The child runs until it exits
// or the GC reaps it; the session is reachable via Get immediately.
func (m *Manager) Create(id string, cfg ptyhost.Config) (*Session, error) {
	pty, err := ptyhost.Spawn(cfg)
	if err != nil {
		return nil, err
	}

	size := cfg.InitialSize
	s := &Session{
		ID:     id,
		pty:    pty,
		screen: vtscreen.New(int(size.Cols), int(size.Rows)),
		status: Connected,
	}
	// When the PTY exits, evict the session from the map. Captured by value so
	// the closure does not pin the Session beyond its lifetime.
	s.onExit = func() { m.remove(id, s) }

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	go s.pumpOutput()
	return s, nil
}

// remove deletes a session from the map, but only if the stored session is the
// one we expect (guards against a races where the id was recreated).
func (m *Manager) remove(id string, s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] == s {
		delete(m.sessions, id)
	}
}

// gcLoop reaps sessions that have been Detached longer than detachTimeout,
// killing the child of each. Runs for the life of the process. Mirrors dinotty
// SessionManager::start_cleanup_task.
func (m *Manager) gcLoop() {
	t := time.NewTicker(m.gcInterval)
	defer t.Stop()
	for range t.C {
		m.reap()
	}
}

// reap performs one GC pass: collect expired detached sessions under the map
// lock, then kill them outside the lock (kill blocks on Wait). Exposed for tests
// to trigger a deterministic pass without waiting for the ticker.
func (m *Manager) reap() {
	now := time.Now()
	var expired []*Session

	m.mu.Lock()
	for id, s := range m.sessions {
		s.mu.Lock()
		expire := s.status == Detached && !s.detached.IsZero() &&
			now.Sub(s.detached) >= m.detachTimeout
		s.mu.Unlock()
		if expire {
			expired = append(expired, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, s := range expired {
		s.kill()
	}
}
