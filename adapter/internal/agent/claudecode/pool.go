package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// warmEntry holds a single pre-warmed claude CLI session that is ready to be
// resumed. The underlying CLI process has already completed its no-op prompt
// and exited; only the session ID (written to disk by claude-code) is kept.
type warmEntry struct {
	cliSessionID string
	cwd          string
	createdAt    time.Time
}

// WarmPool maintains a small pool of pre-warmed claude CLI session IDs.
// Each entry was produced by running `claude -p "echo ok"` during idle time,
// completing the expensive API handshake upfront. When a real request arrives,
// Send() calls Acquire() to pull a matching entry and resumes it with
// `--resume <id>`, cutting first-response latency from 4-7s to ~0.5s.
//
// The pool is safe for concurrent use. Pool size and TTL are controlled by
// BBCLAW_CLAUDE_POOL_SIZE and BBCLAW_CLAUDE_POOL_IDLE_TTL.
type WarmPool struct {
	bin     string
	extra   []string
	size    int
	idleTTL time.Duration
	log     *obs.Logger

	mu      sync.Mutex
	entries []warmEntry

	// replenishCh is a non-blocking signal to the background goroutine that
	// it should top up the pool. Buffered(1) so callers never block.
	replenishCh chan struct{}

	// done is closed when Drain() is called to stop the background goroutine.
	done chan struct{}
	once sync.Once
}

// NewWarmPool creates a WarmPool and starts its background replenish goroutine.
// size=0 disables the pool entirely (Acquire always returns "", false).
func NewWarmPool(bin string, extra []string, size int, idleTTL time.Duration, log *obs.Logger) *WarmPool {
	p := &WarmPool{
		bin:         bin,
		extra:       extra,
		size:        size,
		idleTTL:     idleTTL,
		log:         log,
		replenishCh: make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
	if size > 0 {
		go p.replenishLoop()
		// Kick off an initial fill immediately.
		p.signalReplenish()
	}
	return p
}

// Acquire removes and returns a pre-warmed session ID whose cwd matches the
// requested cwd. Returns ("", false) when the pool is empty, disabled, or no
// entry matches.
func (p *WarmPool) Acquire(cwd string) (string, bool) {
	if p.size == 0 {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for i, e := range p.entries {
		// Skip expired entries — they will be pruned by the replenish loop.
		if p.idleTTL > 0 && now.Sub(e.createdAt) > p.idleTTL {
			continue
		}
		// cwd must match exactly; an empty cwd in the entry matches any request
		// cwd (useful when the pool was seeded without a specific directory).
		if e.cwd != "" && e.cwd != cwd {
			continue
		}
		// Remove from pool (order doesn't matter — swap with last).
		p.entries[i] = p.entries[len(p.entries)-1]
		p.entries = p.entries[:len(p.entries)-1]
		p.log.Infof("claude-code: pool hit cliSession=%s cwd=%q age=%s remaining=%d",
			e.cliSessionID, cwd, now.Sub(e.createdAt).Round(time.Millisecond), len(p.entries))
		// Signal the background goroutine to refill.
		p.signalReplenish()
		return e.cliSessionID, true
	}
	return "", false
}

// Drain stops the background goroutine and discards all pool entries. Safe to
// call multiple times. Should be called during adapter shutdown.
func (p *WarmPool) Drain() {
	p.once.Do(func() {
		close(p.done)
	})
	p.mu.Lock()
	p.entries = nil
	p.mu.Unlock()
}

// signalReplenish sends a non-blocking signal to the replenish loop.
func (p *WarmPool) signalReplenish() {
	select {
	case p.replenishCh <- struct{}{}:
	default:
	}
}

// replenishLoop runs in a background goroutine, topping up the pool whenever
// it receives a signal or the TTL eviction ticker fires.
func (p *WarmPool) replenishLoop() {
	// Eviction ticker: prune stale entries and refill every idleTTL/2 (min 1m).
	evictInterval := p.idleTTL / 2
	if evictInterval < time.Minute {
		evictInterval = time.Minute
	}
	ticker := time.NewTicker(evictInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.evictExpired()
			p.fill()
		case <-p.replenishCh:
			p.fill()
		}
	}
}

// evictExpired removes entries that have exceeded idleTTL.
func (p *WarmPool) evictExpired() {
	if p.idleTTL <= 0 {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.entries[:0]
	for _, e := range p.entries {
		if now.Sub(e.createdAt) <= p.idleTTL {
			kept = append(kept, e)
		} else {
			p.log.Infof("claude-code: pool evict cliSession=%s age=%s (ttl=%s)",
				e.cliSessionID, now.Sub(e.createdAt).Round(time.Second), p.idleTTL)
		}
	}
	p.entries = kept
}

// fill spawns no-op claude processes until the pool reaches its target size.
// Each spawn is sequential to avoid hammering the API on startup.
func (p *WarmPool) fill() {
	for {
		p.mu.Lock()
		need := p.size - len(p.entries)
		p.mu.Unlock()
		if need <= 0 {
			return
		}

		// Check if we've been asked to stop.
		select {
		case <-p.done:
			return
		default:
		}

		entry, err := p.spawnWarm()
		if err != nil {
			p.log.Warnf("claude-code: pool warm failed: %v (will retry on next signal)", err)
			return // back off; next signal or ticker will retry
		}

		p.mu.Lock()
		// Re-check size under lock in case Drain() was called concurrently.
		if len(p.entries) < p.size {
			p.entries = append(p.entries, entry)
			p.log.Infof("claude-code: pool warmed cliSession=%s cwd=%q pool=%d/%d",
				entry.cliSessionID, entry.cwd, len(p.entries), p.size)
		}
		p.mu.Unlock()
	}
}

// noopPrompt is the prompt used to pre-warm a session. It must be:
//   - side-effect free (no file writes, no tool calls)
//   - fast (single-token reply)
//   - unlikely to be confused with real user input
const noopPrompt = "respond with the single word: ready"

// spawnWarm runs `claude -p <noopPrompt> --output-format stream-json --verbose`
// and extracts the cli_session_id from the init event. The process runs to
// completion; only the session ID is retained.
func (p *WarmPool) spawnWarm() (warmEntry, error) {
	args := []string{"-p", noopPrompt, "--output-format", "stream-json", "--verbose"}
	args = append(args, p.extra...)

	// Use a generous timeout for the warm spawn — we don't want a slow API
	// response to block the pool indefinitely, but we also don't want to
	// discard a valid session just because the network was briefly slow.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.bin, args...)
	// Inherit the process environment so API keys are available.
	cmd.Env = os.Environ()
	// No specific cwd for the warm entry — Acquire() matches empty cwd to any
	// request cwd, so the entry is universally reusable.
	cmd.Dir = ""

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return warmEntry{}, fmt.Errorf("stdout pipe: %w", err)
	}
	// Discard stderr to avoid log noise from the no-op prompt.
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return warmEntry{}, fmt.Errorf("start: %w", err)
	}

	// Parse the stream-json output just enough to extract the session_id.
	var cliSessionID string
	dec := json.NewDecoder(stdout)
	for dec.More() {
		var env struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if err := dec.Decode(&env); err != nil {
			break
		}
		if env.Type == "system" && env.Subtype == "init" && env.SessionID != "" {
			cliSessionID = env.SessionID
			break
		}
	}
	// Drain remaining stdout so the process can exit cleanly.
	buf := make([]byte, 4096)
	for {
		_, err := stdout.Read(buf)
		if err != nil {
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		return warmEntry{}, fmt.Errorf("wait: %w", err)
	}
	if cliSessionID == "" {
		return warmEntry{}, fmt.Errorf("no session_id in init event")
	}

	return warmEntry{
		cliSessionID: cliSessionID,
		cwd:          "", // universally reusable
		createdAt:    time.Now(),
	}, nil
}

// Size returns the configured pool capacity.
func (p *WarmPool) Size() int { return p.size }

// Len returns the current number of idle entries in the pool.
func (p *WarmPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// injectEntry adds an entry directly into the pool. Used only by tests.
func (p *WarmPool) injectEntry(e warmEntry) {
	p.mu.Lock()
	p.entries = append(p.entries, e)
	p.mu.Unlock()
}

// poolFromEnv is a helper used by the driver to read pool config from env vars
// and construct a WarmPool. Extracted here so driver.go stays clean.
func poolFromEnv(bin string, extra []string, size int, idleTTL time.Duration, log *obs.Logger) *WarmPool {
	return NewWarmPool(bin, extra, size, idleTTL, log)
}

// stripCCPrefix removes the "cc-" prefix that the adapter mints on session IDs
// before passing them to the claude CLI. Duplicated here (also in driver.go)
// to keep pool.go self-contained.
func stripCCPrefix(id string) string {
	return strings.TrimPrefix(id, "cc-")
}
