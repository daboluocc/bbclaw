package obs

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ringCap bounds the in-memory recent-log buffer surfaced at GET /v1/admin/logs.
// ~1000 lines is enough for the admin page to show a useful tail without the
// operator watching the binary's stdout, while staying tiny in memory.
const ringCap = 1000

type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string

	// ring is a bounded circular buffer of the most recent formatted log lines,
	// independent of where they are also written (stdout / file). rstart marks
	// the oldest entry once the buffer has wrapped (rfull).
	ring   []string
	rstart int
	rfull  bool
}

func NewLogger() *Logger {
	return &Logger{out: os.Stdout, prefix: "bbclaw-adapter", ring: make([]string, 0, ringCap)}
}

// NewLoggerTo builds a Logger that writes to w instead of stdout. The
// `mcp-server` subcommand uses this with os.Stderr because its stdout is the
// MCP JSON-RPC channel and must never carry log lines (ADR-021 §前置闸门).
func NewLoggerTo(w io.Writer) *Logger {
	return &Logger{out: w, prefix: "bbclaw-adapter", ring: make([]string, 0, ringCap)}
}

// Tee mirrors all subsequent log lines to w in addition to the current sink
// (stdout by default). main.go uses this to write a persistent log file under
// the data dir once it's resolved, so logs survive and the admin 日志 page /
// AI can read them without losing stdout. Safe for concurrent use.
func (l *Logger) Tee(w io.Writer) {
	if l == nil || w == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = io.MultiWriter(l.out, w)
}

func (l *Logger) logf(level, format string, args ...any) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("%s ts=%s level=%s %s",
		l.prefix,
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		level,
		fmt.Sprintf(format, args...))
	l.mu.Lock()
	if l.out != nil {
		fmt.Fprintln(l.out, line)
	}
	l.pushRing(line)
	l.mu.Unlock()
}

// pushRing appends line to the bounded ring. Caller holds l.mu.
func (l *Logger) pushRing(line string) {
	if len(l.ring) < ringCap {
		l.ring = append(l.ring, line)
		return
	}
	l.ring[l.rstart] = line
	l.rstart = (l.rstart + 1) % ringCap
	l.rfull = true
}

// Recent returns up to the last n log lines in chronological order. n<=0 returns
// the entire buffer. Used by the admin logs endpoint.
func (l *Logger) Recent(n int) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var ordered []string
	if l.rfull {
		ordered = make([]string, 0, ringCap)
		ordered = append(ordered, l.ring[l.rstart:]...)
		ordered = append(ordered, l.ring[:l.rstart]...)
	} else {
		ordered = append([]string(nil), l.ring...)
	}
	if n > 0 && n < len(ordered) {
		ordered = ordered[len(ordered)-n:]
	}
	return ordered
}

func (l *Logger) Infof(format string, args ...any) {
	l.logf("INFO", format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.logf("WARN", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.logf("ERROR", format, args...)
}

type Metrics struct {
	mu       sync.Mutex
	counters map[string]int64
}

func NewMetrics() *Metrics {
	return &Metrics{counters: make(map[string]int64)}
}

func (m *Metrics) Inc(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[name]++
}

func (m *Metrics) Snapshot() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.counters))
	for k, v := range m.counters {
		out[k] = v
	}
	return out
}
