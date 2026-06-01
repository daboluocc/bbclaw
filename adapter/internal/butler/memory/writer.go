package memory

import (
	"context"
	"time"
)

// Logger is the minimal structured-log surface the writer needs. *obs.Logger
// satisfies it; nil is tolerated (logging becomes a no-op).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Distiller turns one turn's text into a few durable notes. Implementations may
// call an LLM (claudeDistiller) or be a pure stub in tests. It runs only on the
// single background worker, never on the engine's turn path.
type Distiller interface {
	Distill(ctx context.Context, userText, replyText string) ([]Item, error)
}

// gating thresholds (ADR-021 §4 规则门控). Kept simple in v1.
const (
	defaultMinUserChars = 6                // skip trivially short utterances
	defaultQueueDepth   = 8                // bounded; RecordTurn drops when full
	distillTimeout      = 45 * time.Second // hard cap on a single distill call
)

// Writer is the butler MemoryWriter implementation (satisfies butler.MemoryWriter
// structurally: RecordTurn(userText, replyText, cwd string)). It owns a bounded
// channel and exactly one background worker (concurrency=1, ADR-021 §3), so it
// never competes with itself and never blocks the engine. Distillation failures
// are swallowed (log + drop); memory is best-effort and must not affect turns.
type Writer struct {
	store        *Store
	distiller    Distiller
	log          Logger
	ch           chan turnMemo
	minUserChars int
}

type turnMemo struct {
	userText  string
	replyText string
	cwd       string
}

// NewWriter starts the single worker goroutine and returns a ready Writer.
// store and distiller must be non-nil; log may be nil.
func NewWriter(store *Store, distiller Distiller, log Logger) *Writer {
	w := &Writer{
		store:        store,
		distiller:    distiller,
		log:          log,
		ch:           make(chan turnMemo, defaultQueueDepth),
		minUserChars: defaultMinUserChars,
	}
	go w.run()
	return w
}

// RecordTurn enqueues a completed butler turn for async distillation. It is
// non-blocking: when the queue is full the turn is dropped (满即丢) so the
// engine's turn path never stalls on memory work.
func (w *Writer) RecordTurn(userText, replyText, cwd string) {
	select {
	case w.ch <- turnMemo{userText: userText, replyText: replyText, cwd: cwd}:
	default:
		if w.log != nil {
			w.log.Warnf("butler-memory: queue full, dropping turn (cwd=%q)", cwd)
		}
	}
}

func (w *Writer) run() {
	for memo := range w.ch {
		w.process(memo)
	}
}

// process distills one turn and persists the surviving notes. All failures are
// logged and swallowed.
func (w *Writer) process(memo turnMemo) {
	if len([]rune(memo.userText)) < w.minUserChars {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), distillTimeout)
	defer cancel()

	items, err := w.distiller.Distill(ctx, memo.userText, memo.replyText)
	if err != nil {
		if w.log != nil {
			w.log.Warnf("butler-memory: distill failed: %v", err)
		}
		return
	}
	if len(items) == 0 {
		return
	}

	// Store.Append re-applies the poison filter and dedup; this is defence in
	// depth on top of whatever the distiller already filtered.
	if err := w.store.Append(items); err != nil {
		if w.log != nil {
			w.log.Warnf("butler-memory: append failed: %v", err)
		}
		return
	}
	if w.log != nil {
		w.log.Infof("butler-memory: appended %d note(s) cwd=%q", len(items), memo.cwd)
	}
}
