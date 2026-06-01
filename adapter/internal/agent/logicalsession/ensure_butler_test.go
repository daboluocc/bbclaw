package logicalsession

import "testing"

// EnsureButler mints a Role=butler session on first call and returns the same
// session (idempotent) on subsequent calls for the same device+driver.
func TestEnsureButlerIdempotent(t *testing.T) {
	m, _ := newTestManager(t)

	first, err := m.EnsureButler("dev-1", "claude-code", "/ws")
	if err != nil {
		t.Fatalf("EnsureButler first: %v", err)
	}
	if first.Role != RoleButler {
		t.Fatalf("role=%q want %q", first.Role, RoleButler)
	}
	if first.Cwd != "/ws" {
		t.Fatalf("cwd=%q want /ws", first.Cwd)
	}
	if first.DeviceID != "dev-1" || first.Driver != "claude-code" {
		t.Fatalf("device/driver mismatch: %+v", first)
	}

	// A later call with a different cwd must reuse the existing butler (cwd is
	// only honoured at creation time), not mint a second one.
	second, err := m.EnsureButler("dev-1", "claude-code", "/other")
	if err != nil {
		t.Fatalf("EnsureButler second: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second id=%s want same as first=%s", second.ID, first.ID)
	}
	if second.Cwd != "/ws" {
		t.Fatalf("second cwd=%q want /ws (creation cwd preserved)", second.Cwd)
	}

	// Only one butler should exist for the device.
	butlers := 0
	for _, s := range m.List("dev-1", "claude-code", 0) {
		if s.Role == RoleButler {
			butlers++
		}
	}
	if butlers != 1 {
		t.Fatalf("butler count=%d want 1", butlers)
	}
}

// Each device gets its own butler session.
func TestEnsureButlerPerDevice(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.EnsureButler("dev-a", "claude-code", "/ws")
	if err != nil {
		t.Fatalf("EnsureButler a: %v", err)
	}
	b, err := m.EnsureButler("dev-b", "claude-code", "/ws")
	if err != nil {
		t.Fatalf("EnsureButler b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("devices share a butler session id=%s", a.ID)
	}
}

// The butler is excluded from worker-only listings but visible to device-facing
// listings (it is a device-facing session, just a special one).
func TestEnsureButlerIsDeviceFacing(t *testing.T) {
	m, _ := newTestManager(t)
	bsess, err := m.EnsureButler("dev-1", "claude-code", "/ws")
	if err != nil {
		t.Fatalf("EnsureButler: %v", err)
	}
	found := false
	for _, s := range m.ListDeviceFacing("dev-1", "claude-code", 0) {
		if s.ID == bsess.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("butler %s missing from device-facing listing", bsess.ID)
	}
}

func TestEnsureButlerRejectsEmptyDriver(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.EnsureButler("dev-1", "", "/ws"); err == nil {
		t.Fatalf("EnsureButler with empty driver: want error, got nil")
	}
}
