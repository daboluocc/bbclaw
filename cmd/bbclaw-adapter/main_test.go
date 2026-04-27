package main

import (
	"context"
	"sort"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/config"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// fakeDriver is a minimal agent.Driver used to populate the test registry
// so we never spawn a real CLI or network probe during unit tests.
type fakeDriver struct{ name string }

func (f *fakeDriver) Name() string                                              { return f.name }
func (f *fakeDriver) Capabilities() agent.Capabilities                          { return agent.Capabilities{} }
func (f *fakeDriver) Start(context.Context, agent.StartOpts) (agent.SessionID, error) {
	return agent.SessionID(f.name), nil
}
func (f *fakeDriver) Send(agent.SessionID, string) error              { return nil }
func (f *fakeDriver) Events(agent.SessionID) <-chan agent.Event       { ch := make(chan agent.Event); close(ch); return ch }
func (f *fakeDriver) Approve(agent.SessionID, agent.ToolID, agent.Decision) error {
	return agent.ErrUnsupported
}
func (f *fakeDriver) Stop(agent.SessionID) error { return nil }

// makeRegistry builds a deterministic 4-row registry that mirrors the shape
// of k_driver_registry but uses fakeDrivers and per-test-controlled
// predicates. Each row has a forceEnv on the gated drivers (openclaw,
// ollama) just like production.
func makeRegistry(openclawAuto, ollamaAuto bool) []driverReg {
	return []driverReg{
		{
			name: "claude-code",
			construct: func(_ config.Config, _ *obs.Logger) (agent.Driver, error) {
				return &fakeDriver{name: "claude-code"}, nil
			},
			autoEnable: func(config.Config) bool { return true },
		},
		{
			name: "opencode",
			construct: func(_ config.Config, _ *obs.Logger) (agent.Driver, error) {
				return &fakeDriver{name: "opencode"}, nil
			},
			autoEnable: func(config.Config) bool { return true },
		},
		{
			name: "openclaw",
			construct: func(_ config.Config, _ *obs.Logger) (agent.Driver, error) {
				return &fakeDriver{name: "openclaw"}, nil
			},
			autoEnable: func(config.Config) bool { return openclawAuto },
			forceEnv:   "AGENT_OPENCLAW_FORCE",
		},
		{
			name: "ollama",
			construct: func(_ config.Config, _ *obs.Logger) (agent.Driver, error) {
				return &fakeDriver{name: "ollama"}, nil
			},
			autoEnable: func(config.Config) bool { return ollamaAuto },
			forceEnv:   "AGENT_OLLAMA_FORCE",
		},
	}
}

// envFunc returns an os.Getenv-style function backed by a fixed map. Used
// to inject env state without polluting the real process environment.
func envFunc(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// registeredNames returns the sorted set of driver names registered in r.
func registeredNames(r *agent.Router) []string {
	infos := r.List()
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// TestAutoEnableMode: empty AGENT_ENABLED_DRIVERS plus all autoEnable
// predicates truthy should register every driver in the registry.
func TestAutoEnableMode(t *testing.T) {
	logger := obs.NewLogger()
	cfg := config.Config{OpenClawURL: "http://example.invalid"}
	registry := makeRegistry(true, true)
	router := buildAgentRouterFromRegistry(cfg, logger, registry, envFunc(nil))

	got := registeredNames(router)
	want := []string{"claude-code", "ollama", "openclaw", "opencode"} // alphabetic sort
	if !equalStrings(got, want) {
		t.Fatalf("auto mode drivers: got=%v want=%v", got, want)
	}
}

// TestExplicitEnableSubset: AGENT_ENABLED_DRIVERS=claude-code,ollama
// should register only those two, ignoring openclaw even when its cfg/auto
// predicate says yes.
func TestExplicitEnableSubset(t *testing.T) {
	logger := obs.NewLogger()
	cfg := config.Config{OpenClawURL: "http://example.invalid"}
	registry := makeRegistry(true, true)
	env := map[string]string{
		"AGENT_ENABLED_DRIVERS": "claude-code,ollama",
	}
	router := buildAgentRouterFromRegistry(cfg, logger, registry, envFunc(env))

	got := registeredNames(router)
	want := []string{"claude-code", "ollama"}
	if !equalStrings(got, want) {
		t.Fatalf("explicit subset drivers: got=%v want=%v", got, want)
	}
}

// TestForceEnvOverridesAutoSkip: in auto mode (AGENT_ENABLED_DRIVERS empty)
// ollama's TCP probe is faked false, but AGENT_OLLAMA_FORCE=1 should still
// cause registration to be attempted (and succeed, since construct never
// fails in tests).
func TestForceEnvOverridesAutoSkip(t *testing.T) {
	logger := obs.NewLogger()
	cfg := config.Config{}
	registry := makeRegistry(false, false) // both gated drivers auto-skip
	env := map[string]string{
		"AGENT_OLLAMA_FORCE": "1",
	}
	router := buildAgentRouterFromRegistry(cfg, logger, registry, envFunc(env))

	got := registeredNames(router)
	// claude-code + opencode always-on; ollama via force; openclaw still skipped.
	want := []string{"claude-code", "ollama", "opencode"}
	if !equalStrings(got, want) {
		t.Fatalf("force-env drivers: got=%v want=%v", got, want)
	}
}

// TestDefaultDriverExplicit: AGENT_DEFAULT_DRIVER=ollama should make the
// router report ollama as default, overriding the registration-order
// fallback (which would be claude-code).
func TestDefaultDriverExplicit(t *testing.T) {
	logger := obs.NewLogger()
	cfg := config.Config{}
	registry := makeRegistry(false, true)
	env := map[string]string{
		"AGENT_DEFAULT_DRIVER": "ollama",
	}
	router := buildAgentRouterFromRegistry(cfg, logger, registry, envFunc(env))

	if got := router.DefaultName(); got != "ollama" {
		t.Fatalf("default driver: got=%q want=%q", got, "ollama")
	}
}

// TestDefaultDriverFallback: with AGENT_DEFAULT_DRIVER unset the first
// successfully-registered row in the registry (claude-code) should be the
// default.
func TestDefaultDriverFallback(t *testing.T) {
	logger := obs.NewLogger()
	cfg := config.Config{}
	registry := makeRegistry(false, true)
	router := buildAgentRouterFromRegistry(cfg, logger, registry, envFunc(nil))

	if got := router.DefaultName(); got != "claude-code" {
		t.Fatalf("default driver fallback: got=%q want=%q", got, "claude-code")
	}
}

// equalStrings compares two already-sorted string slices for equality.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
