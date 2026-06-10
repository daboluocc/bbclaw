package driverstate

import (
	"path/filepath"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// TestButlerDriverRoundTrip verifies butler_driver persists independently of
// active_driver and survives a reload (ADR-023).
func TestButlerDriverRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver_state.json")
	st, err := NewStore(path, obs.NewLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if st.ButlerDriver() != "" {
		t.Errorf("fresh store ButlerDriver should be empty, got %q", st.ButlerDriver())
	}

	if err := st.SetActiveDriver("opencode"); err != nil {
		t.Fatalf("SetActiveDriver: %v", err)
	}
	if err := st.SetButlerDriver("claude-code"); err != nil {
		t.Fatalf("SetButlerDriver: %v", err)
	}

	// The two settings are independent.
	if st.ActiveDriver() != "opencode" {
		t.Errorf("active_driver: want opencode, got %q", st.ActiveDriver())
	}
	if st.ButlerDriver() != "claude-code" {
		t.Errorf("butler_driver: want claude-code, got %q", st.ButlerDriver())
	}

	// Reload from disk — both fields must survive.
	st2, err := NewStore(path, obs.NewLogger())
	if err != nil {
		t.Fatalf("reload NewStore: %v", err)
	}
	if st2.ActiveDriver() != "opencode" {
		t.Errorf("reloaded active_driver: want opencode, got %q", st2.ActiveDriver())
	}
	if st2.ButlerDriver() != "claude-code" {
		t.Errorf("reloaded butler_driver: want claude-code, got %q", st2.ButlerDriver())
	}

	// Clearing butler_driver reverts to empty (caller applies the fallback).
	if err := st2.SetButlerDriver(""); err != nil {
		t.Fatalf("clear SetButlerDriver: %v", err)
	}
	if st2.ButlerDriver() != "" {
		t.Errorf("after clear, butler_driver should be empty, got %q", st2.ButlerDriver())
	}
}
