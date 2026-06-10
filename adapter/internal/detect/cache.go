package detect

import (
	"sync"
	"time"
)

// Driver-detection cache (ADR-023). DetectAll does a LookPath sweep plus a
// 500ms TCP dial for ollama; the admin page and the cloud drivers-reply both
// want per-driver "installed" status and may call repeatedly. Caching the
// snapshot process-wide for a short TTL keeps polling from stacking ollama
// dials. Detection is host state (not per-request), so a package singleton is
// the right scope, and it gives both the LAN HTTP layer and the cloud relay a
// single source of truth for the driver name → installed mapping.
const cacheTTL = 3 * time.Second

var (
	cacheMu      sync.Mutex
	cacheVal     *Environment
	cacheExpires time.Time
)

// CachedEnvironment returns a cached full detection snapshot, refreshing when
// the TTL has lapsed. The doubao import path is not relevant to driver
// detection, so it is run with an empty source path.
func CachedEnvironment() *Environment {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheVal != nil && time.Now().Before(cacheExpires) {
		return cacheVal
	}
	cacheVal = DetectAll("")
	cacheExpires = time.Now().Add(cacheTTL)
	return cacheVal
}

// InstalledByDriver maps each known agent driver name (matching Driver.Name())
// to whether its backing CLI/service is present on the host, using each
// driver's own detection logic (LookPath for the CLIs, a TCP probe for ollama,
// a config file for openclaw). Drivers not covered are absent from the map so
// callers can omit `installed` rather than misreport false.
func InstalledByDriver() map[string]bool {
	env := CachedEnvironment()
	return map[string]bool{
		"claude-code": env.ClaudeCode.Present,
		"opencode":    env.OpenCode.Present,
		"aider":       env.Aider.Present,
		"ollama":      env.Ollama.Present,
		"openclaw":    env.OpenClaw.Present,
		"codex":       env.Codex.Present,
	}
}
