package claudecode

import (
	"bytes"
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
// Entries are bound to a specific working directory so the pre-warmed
// claude-code subprocess records its session JSONL under the same project
// directory that the real Send() spawn will inherit. Acquire strict-matches
// the entry cwd against the request cwd; mismatched requests fall through to
// cold spawn.
//
// The pool warms several cwds independently (warmCwds): the configured
// default project plus the per-device butler workspace (ADR-021 §3), each
// kept topped up to `size` idle entries so a butler turn hits a warm session
// every round instead of paying the 4-7s cold-start.
//
// The pool is safe for concurrent use. Pool size and TTL are controlled by
// BBCLAW_CLAUDE_POOL_SIZE and BBCLAW_CLAUDE_POOL_IDLE_TTL. The cwds are
// passed in at construction (see NewWarmPool).
type WarmPool struct {
	bin      string
	extra    []string
	env      map[string]string // driver-level env overrides injected into warm spawns
	warmCwds []string          // working directories warmed independently; each entry is stamped with its cwd
	size     int               // target idle entries PER cwd
	idleTTL  time.Duration
	log      *obs.Logger

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
//
// warmCwds are the working directories spawnWarm uses; each entry is stamped
// with the cwd it was warmed in and Acquire strict-matches against it. Pass the
// canonical project path(s) (e.g. BBCLAW_DEFAULT_CWD / CwdPool[0].Path plus the
// butler workspace) — an empty entry makes that warm process inherit the
// adapter's own cwd, which is rarely what callers want when CWD_POOL is
// configured. Duplicate and blank-only entries are de-duplicated; if none
// remain the pool warms a single inherited-cwd ("") slot for legacy parity.
func NewWarmPool(bin string, extra []string, env map[string]string, warmCwds []string, size int, idleTTL time.Duration, log *obs.Logger) *WarmPool {
	p := &WarmPool{
		bin:         bin,
		extra:       extra,
		env:         env,
		warmCwds:    dedupeCwds(warmCwds),
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
		// Strict cwd match. The previous "empty matches any" rule allowed a
		// warm entry spawned in the adapter's launch directory to be reused
		// for sessions targeting a CWD_POOL path, which leaked the adapter's
		// path into claude-code's session JSONL and confused the model about
		// which project it was working in.
		if e.cwd != cwd {
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

// fill spawns no-op claude processes until every warm cwd has `size` idle
// entries. Each spawn is sequential to avoid hammering the API on startup.
func (p *WarmPool) fill() {
	for _, cwd := range p.warmCwds {
		for {
			p.mu.Lock()
			have := p.countForCwd(cwd)
			p.mu.Unlock()
			if have >= p.size {
				break
			}

			// Check if we've been asked to stop.
			select {
			case <-p.done:
				return
			default:
			}

			entry, err := p.spawnWarm(cwd)
			if err != nil {
				p.log.Warnf("claude-code: pool warm failed cwd=%q: %v (will retry on next signal)", cwd, err)
				break // back off this cwd; next signal or ticker will retry
			}

			p.mu.Lock()
			// Re-check size under lock in case Drain() was called concurrently.
			if p.countForCwd(cwd) < p.size {
				p.entries = append(p.entries, entry)
				p.log.Infof("claude-code: pool warmed cliSession=%s cwd=%q pool=%d/%d",
					entry.cliSessionID, entry.cwd, p.countForCwd(cwd), p.size)
			}
			p.mu.Unlock()
		}
	}
}

// countForCwd returns the number of idle entries stamped with cwd. Caller must
// hold p.mu.
func (p *WarmPool) countForCwd(cwd string) int {
	n := 0
	for _, e := range p.entries {
		if e.cwd == cwd {
			n++
		}
	}
	return n
}

// noopPrompt is the prompt used to pre-warm a session. It must be:
//   - side-effect free (no file writes, no tool calls)
//   - fast (single-token reply)
//   - unlikely to be confused with real user input
const noopPrompt = "respond with the single word: ready"

// spawnWarm runs `claude -p <noopPrompt> --output-format stream-json --verbose`
// in cwd and extracts the cli_session_id from the init event. The process runs
// to completion; only the session ID is retained.
func (p *WarmPool) spawnWarm(cwd string) (warmEntry, error) {
	args := []string{"-p", noopPrompt, "--output-format", "stream-json", "--verbose"}
	args = append(args, p.extra...)

	// Use a generous timeout for the warm spawn — we don't want a slow API
	// response to block the pool indefinitely, but we also don't want to
	// discard a valid session just because the network was briefly slow.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.bin, args...)
	// Inject driver-level env overrides (e.g. ANTHROPIC_BASE_URL) on top of
	// the inherited process environment so warm sessions use the same endpoint
	// as regular Send() spawns.
	if len(p.env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), p.env)
	} else {
		cmd.Env = os.Environ()
	}
	// Pin the warm spawn to the requested cwd so its session JSONL lands in
	// the same project directory that real Send() spawns will inherit. An empty
	// cwd falls back to Go's "inherit parent cwd" behaviour, which is fine for
	// deployments that haven't configured CWD_POOL.
	cmd.Dir = cwd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return warmEntry{}, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

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
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return warmEntry{}, fmt.Errorf("wait: %w; stderr: %s", err, stderr)
		}
		return warmEntry{}, fmt.Errorf("wait: %w", err)
	}
	if cliSessionID == "" {
		return warmEntry{}, fmt.Errorf("no session_id in init event")
	}

	return warmEntry{
		cliSessionID: cliSessionID,
		cwd:          cwd,
		createdAt:    time.Now(),
	}, nil
}

// dedupeCwds removes duplicate working directories while preserving order. When
// the input is empty it returns a single inherited-cwd ("") slot so the pool
// keeps the legacy "warm one entry in the adapter's own cwd" behaviour.
func dedupeCwds(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
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
