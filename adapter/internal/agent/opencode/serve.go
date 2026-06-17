package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// ADR-031: the serve-backed opencode driver talks to a long-lived
// `opencode serve` (OpenAPI 3.1) over its official Go SDK, instead of spawning
// `opencode run` and scraping NDJSON per turn. serveManager owns that single
// shared server process: it starts it lazily, performs the version handshake
// (the contract that protects us from API drift when the user brings their own
// opencode), and exposes the base URL the SDK client points at.

// Supported opencode server version range. The user installs opencode
// themselves (BYO, same as `claude`); we verify at runtime via GET
// /global/health and refuse to drive a version we have not validated, rather
// than silently misparse a drifted event schema. Bump ocMaxMajor/Minor when a
// newer line has been tested. ocMinVersion is inclusive.
const (
	ocMinVersion = "1.15.0"
	// ocMaxMinorExclusive: versions with minor strictly below this are accepted
	// within major 1 (i.e. 1.15.x .. 1.<max-1>.x). Keep generous; tighten only
	// if a future minor breaks the event/part contract.
	ocSupportedMajor    = 1
	ocMinMinor          = 15
	ocMaxMinorExclusive = 30
)

type serveManager struct {
	bin string
	log *obs.Logger

	// configContent, when non-empty, is passed as OPENCODE_CONFIG_CONTENT to the
	// serve process (butler MCP servers + instructions). Set before first start.
	configContent string

	// providerEnv is merged onto os.Environ for the serve process (ADR-031 P1-3):
	// the scoped channel for provider credentials. Set before first start.
	providerEnv map[string]string

	mu         sync.Mutex
	started    bool
	baseURL    string
	version    string
	cmd        *exec.Cmd
	rootCtx    context.Context
	failures   int       // consecutive spawn/crash failures (reset on healthy start)
	lastSpawn  time.Time // when the last spawn was attempted (for respawn backoff)
	versionErr error     // last version-handshake rejection (surfaced to admin, P2-5)
}

const (
	respawnBaseBackoff = 500 * time.Millisecond
	respawnMaxBackoff  = 16 * time.Second
)

func newServeManager(bin string, log *obs.Logger) *serveManager {
	if strings.TrimSpace(bin) == "" {
		bin = defaultBin
	}
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	return &serveManager{bin: bin, log: log}
}

// ensure starts the serve process (once) and returns its base URL. Safe to call
// concurrently; only the first caller spawns. ctx is the driver-lifetime
// context used to tie the serve process lifecycle to the driver.
func (m *serveManager) ensure(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started && m.alive() {
		return m.baseURL, nil
	}

	// Respawn backoff: after a crash/failed start, don't spawn-storm. Grow the
	// minimum interval with consecutive failures (reset on a healthy start).
	if m.failures > 0 && !m.lastSpawn.IsZero() {
		backoff := respawnBackoff(m.failures)
		if wait := backoff - time.Since(m.lastSpawn); wait > 0 {
			return "", fmt.Errorf("opencode serve: backing off after %d failure(s); retry in %s", m.failures, wait.Round(time.Millisecond))
		}
	}
	m.lastSpawn = time.Now()

	port, err := freeTCPPort()
	if err != nil {
		return "", fmt.Errorf("opencode serve: pick port: %w", err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command(m.bin, "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port))
	cmd.Env = buildServeEnv(os.Environ(), m.configContent, m.providerEnv)
	cmd.Stdout = serveLogWriter{m.log, "stdout"}
	cmd.Stderr = serveLogWriter{m.log, "stderr"}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("opencode serve: start %s: %w", m.bin, err)
	}

	ver, err := waitHealthy(ctx, base, 20*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		m.failures++
		return "", fmt.Errorf("opencode serve: health handshake: %w", err)
	}
	if verr := checkVersion(ver); verr != nil {
		// Refuse rather than misparse a drifted schema (ADR-031 §2).
		_ = cmd.Process.Kill()
		m.failures++
		m.versionErr = verr
		return "", verr
	}

	m.cmd = cmd
	m.baseURL = base
	m.version = ver
	m.started = true
	m.failures = 0 // healthy start clears the backoff
	m.versionErr = nil
	m.rootCtx = ctx
	m.log.Infof("opencode serve: ready pid=%d %s version=%s", cmd.Process.Pid, base, ver)

	// Reap on exit so a crash flips started=false and the next ensure respawns.
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd {
			m.started = false
			m.failures++ // a crash counts toward respawn backoff
			m.log.Warnf("opencode serve: process pid=%d exited; will respawn on next use", cmd.Process.Pid)
		}
		m.mu.Unlock()
	}()
	return base, nil
}

func (m *serveManager) alive() bool {
	return m.cmd != nil && m.cmd.Process != nil
}

func (m *serveManager) currentVersion() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// versionError returns the last version-handshake rejection, if any (nil once a
// supported serve started). Surfaced to the admin page (P2-5).
func (m *serveManager) versionError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.versionErr
}

// respawnBackoff is the minimum interval before re-spawning after `failures`
// consecutive failures: 500ms, 1s, 2s, … capped at 16s.
func respawnBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := respawnBaseBackoff << (failures - 1)
	if d > respawnMaxBackoff || d <= 0 {
		d = respawnMaxBackoff
	}
	return d
}

func (m *serveManager) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
	m.started = false
}

// ── helpers ─────────────────────────────────────────────────────────────────

// buildServeEnv assembles the serve process environment: the inherited base,
// plus OPENCODE_CONFIG_CONTENT (butler config) and provider credentials. Later
// entries override earlier ones (Go exec keeps the last value per key), so
// providerEnv and configContent take precedence over inherited values.
func buildServeEnv(base []string, configContent string, providerEnv map[string]string) []string {
	out := append([]string(nil), base...)
	if configContent != "" {
		out = append(out, "OPENCODE_CONFIG_CONTENT="+configContent)
	}
	for k, v := range providerEnv {
		if k != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(ctx context.Context, base string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/global/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var body struct {
				Version string `json:"version"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if body.Version != "" {
				return body.Version, nil
			}
			return "unknown", nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("not healthy within %s", timeout)
}

// checkVersion enforces the supported range (ADR-031 §2). It is deliberately
// lenient on parse failures (accept "unknown"/odd formats with a warning rather
// than hard-fail) — the goal is to catch KNOWN-incompatible versions, not to be
// a strict semver gate.
func checkVersion(ver string) error {
	maj, min, ok := parseMajorMinor(ver)
	if !ok {
		return nil // unparseable → allow, the handshake at least proved reachability
	}
	if maj != ocSupportedMajor || min < ocMinMinor || min >= ocMaxMinorExclusive {
		return fmt.Errorf("opencode version %s is outside the supported range [%s, %d.%d); "+
			"install a compatible opencode (e.g. `brew install opencode`) or bump the adapter's pin",
			ver, ocMinVersion, ocSupportedMajor, ocMaxMinorExclusive)
	}
	return nil
}

func parseMajorMinor(ver string) (maj, min int, ok bool) {
	v := strings.TrimPrefix(strings.TrimSpace(ver), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	var e1, e2 error
	maj, e1 = atoi(parts[0])
	min, e2 = atoi(parts[1])
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

type serveLogWriter struct {
	log    *obs.Logger
	stream string
}

func (w serveLogWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			w.log.Infof("opencode serve[%s]: %s", w.stream, line)
		}
	}
	return len(b), nil
}
