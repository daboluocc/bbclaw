// Command cancelprobe is a MANUAL, real-claude PTY probe for ADR-041 §0 — run it
// by hand to (re)confirm claude's barge-in/interrupt behaviour when its TUI
// changes. It is never run by `go test` (it spawns a real claude and costs API).
//
//	CLAUDE_BIN=/path/to/claude go run ./cmd/cancelprobe
//
// What it established (ADR-041 §0):
//   - one ESC interrupts cleanly; the composer empties (no redirect trap);
//   - "Interrupted · What should Claude do instead?" is passive scrollback, not a
//     dismissable modal;
//   - the 串轮 bug = a barge-in injected too soon after ESC ("ESC + 60ms +
//     transcript\r") does NOT submit, lingers in the composer, and the NEXT
//     barge-in APPENDS to it → one merged garbled turn;
//   - the fix = ESC → settle → Ctrl-U (clear composer) → transcript → pause →
//     Enter, which makes the latest barge-in REPLACE the un-submitted one.
//
// It runs the bug repro and the fix back-to-back and prints whether each merged.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/ptyhost"
	vtscreen "github.com/zhoushoujianwork/agent-runner/termscreen"
)

const (
	esc  = "\x1b" // ESC: interrupt
	ku   = "\x15" // Ctrl-U: kill the composer line
	enter = "\r"
)

func main() {
	cli := os.Getenv("CLAUDE_BIN")
	if cli == "" {
		cli = "claude"
	}

	fmt.Println("### REPRO (old timing: ESC + 60ms + 'text\\r', rapid x2) — expect MERGED")
	merged1 := runBargeIn(cli, false)
	fmt.Printf(">>> repro merged=%v (want true)\n\n", merged1)

	fmt.Println("### FIX (ESC + settle + Ctrl-U + 'text' + pause + Enter, rapid x2) — expect SEPARATE")
	merged2 := runBargeIn(cli, true)
	fmt.Printf(">>> fix merged=%v (want false)\n", merged2)
}

// runBargeIn spawns a fresh claude, starts a long generation, then fires two
// rapid back-to-back barge-ins (MANGO then KIWI). With fix=false it uses the old
// glued "ESC+60ms+text\r"; with fix=true the ADR-041 ESC→settle→Ctrl-U→text→Enter.
// Returns whether the two transcripts merged into one composer line (the 串轮).
func runBargeIn(cli string, fix bool) bool {
	dir, _ := os.MkdirTemp("", "cancelprobe")
	defer os.RemoveAll(dir)
	p, err := ptyhost.Spawn(ptyhost.Config{
		Argv: []string{cli, "--dangerously-skip-permissions"}, Cwd: dir,
		InitialSize: ptyhost.Size{Cols: 80, Rows: 24},
	})
	if err != nil {
		fmt.Println("spawn error:", err)
		return false
	}
	defer p.Close()

	scr := vtscreen.New(80, 24)
	var mu sync.Mutex
	go func() {
		buf := make([]byte, 8192)
		for {
			n, e := p.Read(buf)
			if n > 0 {
				mu.Lock()
				scr.Feed(buf[:n])
				mu.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	vis := func() string { mu.Lock(); defer mu.Unlock(); return scr.VisibleText() }
	w := func(s string) { _, _ = p.Write([]byte(s)) }

	// Onboarding + a long generation to interrupt.
	time.Sleep(3 * time.Second)
	for i := 0; i < 4 && !strings.Contains(vis(), "esc to interrupt"); i++ {
		w(enter)
		time.Sleep(900 * time.Millisecond)
	}
	w("Count slowly from 1 to 60, one integer per line, each with a one-word note.")
	time.Sleep(800 * time.Millisecond)
	w(enter)
	time.Sleep(5 * time.Second)

	bargeIn := func(text string) {
		if fix {
			w(esc)
			time.Sleep(250 * time.Millisecond)
			w(ku)
			time.Sleep(120 * time.Millisecond)
			w(text)
			time.Sleep(120 * time.Millisecond)
			w(enter)
		} else {
			w(esc)
			time.Sleep(60 * time.Millisecond)
			w(text + enter)
		}
	}
	bargeIn("Reply with exactly the single word MANGO.")
	time.Sleep(900 * time.Millisecond)
	bargeIn("Reply with exactly the single word KIWI.")
	time.Sleep(6 * time.Second)

	v := vis()
	fmt.Printf("--- final screen ---\n%s\n", v)
	return strings.Contains(v, "MANGO.Reply") || strings.Contains(v, "MANGOReply")
}
