package proactive

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// TestRunOnceRealClaude is an opt-in integration test: it spawns a REAL claude
// worker and runs one proactive task turn, proving the §3.3 runner end-to-end
// (spawn → warmup → turn → reply capture) against the actual CLI — not a mock.
//
// Gated on env so `go test ./...` stays hermetic (no claude / auth in CI):
//
//	PROACTIVE_REAL_CLAUDE=/path/to/claude go test ./internal/proactive/ -run RealClaude -v
func TestRunOnceRealClaude(t *testing.T) {
	claude := os.Getenv("PROACTIVE_REAL_CLAUDE")
	if claude == "" {
		t.Skip("set PROACTIVE_REAL_CLAUDE=/path/to/claude to run the live worker test")
	}
	cwd := t.TempDir()
	baseArgv := butler.DeviceClaudeArgs([]string{claude}, cwd, nil)
	cfg := butler.WorkerConfig(baseArgv, cwd)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	mgr := session.NewManager()
	r, err := New(ctx, mgr, "proactive-probe", cfg, session.DefaultGridCols, session.DefaultGridRows)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	reply, err := r.RunOnce(ctx, "用一句话回答：一加一等于几", 90*time.Second)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	t.Logf("worker reply: %q", reply)
	if reply == "" {
		t.Fatal("worker returned an empty reply")
	}
}
