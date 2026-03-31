package obs

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

type Logger struct {
	base   *log.Logger
	prefix string
}

func NewLogger() *Logger {
	return &Logger{
		base:   log.New(os.Stdout, "", 0),
		prefix: "bbclaw-adapter",
	}
}

func (l *Logger) logf(level, format string, args ...any) {
	if l == nil || l.base == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.base.Printf("%s ts=%s level=%s %s",
		l.prefix,
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		level,
		msg)
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
