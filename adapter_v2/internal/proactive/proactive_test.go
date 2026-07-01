package proactive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// mockCLI echoes each injected line as "ANSWER: <line>" on a stable row, mirroring
// the deviceapi test harness so the Bridge's boundary detection + extraction fire.
const mockCLI = `
printf '\033[5;1H> '
while IFS= read -r line; do
  line="${line#$'\033'}"
  printf '\033[10;1H\033[2K\033[2m. Working... (1s esc to interrupt)\033[0m'
  printf '\033[7;1H\033[2KANSWER: %s' "$line"
  printf '\033[10;1H\033[2K'
  printf '\033[5;1H\033[2K> '
done
`

func newMockRunner(t *testing.T) *Runner {
	t.Helper()
	m := session.NewManager()
	s, err := m.Create("reminder-worker-test", ptyhost.Config{
		Argv:        []string{"bash", "-c", mockCLI},
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("create mock session: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// warmup off: the mock has no claude startup handshake.
	return newRunner(ctx, s, 80, 24, false)
}

func TestRunOnceReturnsReply(t *testing.T) {
	r := newMockRunner(t)
	reply, err := r.RunOnce(context.Background(), "check the flash log", 5*time.Second)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !strings.Contains(reply, "ANSWER: check the flash log") {
		t.Fatalf("reply = %q, want it to contain the echoed task", reply)
	}
}

func TestRunOnceSerializes(t *testing.T) {
	r := newMockRunner(t)
	// Hold the runner busy by starting a turn in the background, then assert a
	// concurrent RunOnce is rejected with ErrBusy (one worker turn at a time).
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = r.RunOnce(context.Background(), "first task", 5*time.Second)
	}()
	<-started
	// Give the goroutine a moment to claim the running flag before we race it.
	time.Sleep(50 * time.Millisecond)
	if _, err := r.RunOnce(context.Background(), "second task", 5*time.Second); err != ErrBusy {
		t.Fatalf("concurrent RunOnce err = %v, want ErrBusy", err)
	}
}

func TestRunOnceContextCancel(t *testing.T) {
	r := newMockRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if _, err := r.RunOnce(ctx, "task", 5*time.Second); err == nil {
		t.Fatal("RunOnce with cancelled ctx should error")
	}
}
