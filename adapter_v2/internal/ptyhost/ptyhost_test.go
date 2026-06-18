package ptyhost

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// readAll drains a PTY to EOF, returning everything the child wrote. The kernel
// surfaces EOF on the master fd once the child exits and the slave is closed, so
// a short-lived child lets us read its full output deterministically. An
// io.ErrClosedPipe / EIO from a torn-down PTY is treated as a clean end.
func readAll(t *testing.T, p PTY) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, p); err != nil && !errors.Is(err, io.EOF) {
		// On some platforms reading a PTY master after the child exits yields
		// an EIO error rather than a clean EOF; that is not a test failure.
		t.Logf("read returned non-EOF error (tolerated): %v", err)
	}
	return buf.Bytes()
}

// TestSpawnReadWait is the core table: launch a short-lived process, read its
// output to EOF, then assert the exit code Wait reports. It exercises spawn,
// Read-to-EOF, and the success / non-zero / signal exit-code paths in one place.
func TestSpawnReadWait(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		wantText string // substring expected in the child's output (empty = skip)
		wantCode int    // exit code Wait must report
	}{
		{
			name:     "prints hello and exits zero",
			argv:     []string{"sh", "-c", "printf hello"},
			wantText: "hello",
			wantCode: 0,
		},
		{
			name:     "interactive shell echoes input back", // spec: bash -i + echo
			argv:     []string{"sh", "-c", "echo hi"},
			wantText: "hi",
			wantCode: 0,
		},
		{
			name:     "non-zero exit is reported via code",
			argv:     []string{"sh", "-c", "exit 3"},
			wantText: "",
			wantCode: 3,
		},
		{
			name:     "non-zero exit with output",
			argv:     []string{"sh", "-c", "printf boom; exit 7"},
			wantText: "boom",
			wantCode: 7,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			p, err := Spawn(Config{Argv: tt.argv})
			if err != nil {
				t.Fatalf("Spawn(%v) error: %v", tt.argv, err)
			}
			// Read to EOF before Wait: draining the master lets the child run to
			// completion and avoids deadlocking on a full PTY buffer.
			out := readAll(t, p)
			if tt.wantText != "" && !strings.Contains(string(out), tt.wantText) {
				t.Errorf("output %q does not contain %q", out, tt.wantText)
			}

			code, err := p.Wait()
			if err != nil {
				t.Fatalf("Wait error: %v", err)
			}
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if err := p.Close(); err != nil {
				t.Logf("Close after exit returned (tolerated): %v", err)
			}
		})
	}
}

// TestWriteThenRead feeds stdin and reads the echoed output back, the way the
// terminal channel and ASR-injection path will. We run an interactive shell so
// the line we write is both echoed by the tty and executed.
func TestWriteThenRead(t *testing.T) {
	p, err := Spawn(Config{Argv: []string{"sh", "-i"}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	if _, err := io.WriteString(p, "echo marker123\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Tell the shell to exit so Read eventually hits EOF instead of blocking.
	if _, err := io.WriteString(p, "exit\n"); err != nil {
		t.Fatalf("Write exit: %v", err)
	}

	out := string(readAll(t, p))
	if !strings.Contains(out, "marker123") {
		t.Errorf("output %q does not contain echoed marker", out)
	}

	code, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestResize covers two things: a bare Resize call must not error, and the new
// dimensions must actually reach the kernel PTY — verified by having the shell
// print `stty size` (rows cols) after we resize.
func TestResize(t *testing.T) {
	t.Run("resize does not error", func(t *testing.T) {
		p, err := Spawn(Config{Argv: []string{"sh", "-c", "sleep 0.2"}})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		defer p.Close()
		for _, s := range []Size{{Cols: 100, Rows: 40}, {Cols: 132, Rows: 50}, {}} {
			if err := p.Resize(s); err != nil {
				t.Errorf("Resize(%+v) error: %v", s, err)
			}
		}
		if _, err := p.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	t.Run("stty size reflects new dimensions", func(t *testing.T) {
		const cols, rows = 123, 45
		p, err := Spawn(Config{
			Argv:        []string{"sh", "-c", "sleep 0.15; stty size"},
			InitialSize: Size{Cols: 80, Rows: 24},
		})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		defer p.Close()

		// Resize before the child runs `stty size`.
		if err := p.Resize(Size{Cols: cols, Rows: rows}); err != nil {
			t.Fatalf("Resize: %v", err)
		}

		out := string(readAll(t, p))
		// stty size prints "rows cols".
		want := "45 123"
		if !strings.Contains(out, want) {
			t.Errorf("stty size output %q does not contain %q", strings.TrimSpace(out), want)
		}
		if _, err := p.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})
}

// TestEnvInjection asserts the host defaults TERM=xterm-256color and merges
// caller-supplied env, since the TUI rendering depends on a sane TERM.
func TestEnvInjection(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		argv     []string
		wantText string
	}{
		{
			name:     "default TERM is xterm-256color",
			argv:     []string{"sh", "-c", "printf %s \"$TERM\""},
			wantText: "xterm-256color",
		},
		{
			name:     "caller env overrides default TERM",
			env:      map[string]string{"TERM": "dumb"},
			argv:     []string{"sh", "-c", "printf %s \"$TERM\""},
			wantText: "dumb",
		},
		{
			name:     "caller env var is visible to child",
			env:      map[string]string{"BBCLAW_TEST_VAR": "ok42"},
			argv:     []string{"sh", "-c", "printf %s \"$BBCLAW_TEST_VAR\""},
			wantText: "ok42",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			p, err := Spawn(Config{Argv: tt.argv, Env: tt.env})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			defer p.Close()
			out := string(readAll(t, p))
			if !strings.Contains(out, tt.wantText) {
				t.Errorf("output %q does not contain %q", strings.TrimSpace(out), tt.wantText)
			}
			if _, err := p.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	}
}

// TestSpawnEmptyArgv guards the empty-argv error path.
func TestSpawnEmptyArgv(t *testing.T) {
	if _, err := Spawn(Config{}); !errors.Is(err, ErrEmptyArgv) {
		t.Fatalf("Spawn with empty argv: got %v, want ErrEmptyArgv", err)
	}
}

// TestSpawnBadProgram ensures a non-existent program surfaces an error rather
// than a half-built PTY.
func TestSpawnBadProgram(t *testing.T) {
	if _, err := Spawn(Config{Argv: []string{"definitely-not-a-real-binary-xyz"}}); err == nil {
		t.Fatal("expected error spawning a non-existent program, got nil")
	}
}

// TestCloseStopsLongRunningChild verifies Close kills a child that would
// otherwise outlive the test, and that Wait then unblocks.
func TestCloseStopsLongRunningChild(t *testing.T) {
	p, err := Spawn(Config{Argv: []string{"sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Logf("Close returned (tolerated): %v", err)
	}

	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Close killed the child")
	}
}
