package obs

import (
	"log"
	"os"
	"sync"
)

type Logger struct {
	base *log.Logger
}

func NewLogger() *Logger {
	return &Logger{base: log.New(os.Stdout, "bbclaw-adapter ", log.LstdFlags|log.LUTC)}
}

func (l *Logger) Infof(format string, args ...any) {
	l.base.Printf("INFO "+format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.base.Printf("WARN "+format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.base.Printf("ERROR "+format, args...)
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
