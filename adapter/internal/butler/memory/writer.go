package memory

import (
	"context"
	"sync"
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
	consolidateTimeout  = 90 * time.Second // hard cap on a single consolidation
	// defaultTickInterval is how often the idle/backstop trigger is evaluated
	// (ADR-022 §1). Threshold is checked synchronously after each turn; the
	// ticker covers the time-based classes that fire without new turns.
	defaultTickInterval = 60 * time.Second
)

// Writer is the butler MemoryWriter implementation (satisfies butler.MemoryWriter
// structurally: RecordTurn(userText, replyText, cwd string)). It owns a bounded
// channel and exactly one background worker (concurrency=1, ADR-021 §3), so it
// never competes with itself and never blocks the engine. Distillation failures
// are swallowed (log + drop); memory is best-effort and must not affect turns.
//
// When a Consolidator is attached (ADR-022), the same single worker also runs
// the second-layer "沉淀" pass — per-turn append and consolidation are naturally
// serialized on one worker, so the read-clear vs append race cannot occur
// concurrently. A lightweight ticker drives the time-based (idle/backstop)
// triggers; the threshold trigger is checked right after each turn append.
type Writer struct {
	store        *Store
	distiller    Distiller
	log          Logger
	ch           chan turnMemo
	minUserChars int

	// consolidation (optional; nil = disabled, ADR-022). Set before start().
	consolidator  *Consolidator
	trig          triggerConfig
	tickInterval  time.Duration
	consolidateCh chan struct{}
	now           func() time.Time

	mu                sync.Mutex
	lastTurnAt        time.Time
	lastConsolidateAt time.Time
}

type turnMemo struct {
	userText  string
	replyText string
	cwd       string
}

// newWriter builds a Writer without starting the worker, so callers can attach
// consolidation before start(). store and distiller must be non-nil.
func newWriter(store *Store, distiller Distiller, log Logger) *Writer {
	return &Writer{
		store:         store,
		distiller:     distiller,
		log:           log,
		ch:            make(chan turnMemo, defaultQueueDepth),
		minUserChars:  defaultMinUserChars,
		consolidateCh: make(chan struct{}, 1),
		tickInterval:  defaultTickInterval,
		now:           time.Now,
	}
}

// NewWriter starts the single worker goroutine and returns a ready Writer with
// consolidation disabled. store and distiller must be non-nil; log may be nil.
func NewWriter(store *Store, distiller Distiller, log Logger) *Writer {
	w := newWriter(store, distiller, log)
	w.start()
	return w
}

func (w *Writer) start() {
	go w.run()
}

// RecordTurn enqueues a completed butler turn for async distillation. It is
// non-blocking: when the queue is full the turn is dropped (满即丢) so the
// engine's turn path never stalls on memory work. It also stamps the turn-end
// timestamp used by the idle trigger (ADR-022 §1) — a single cheap assignment,
// keeping the non-blocking contract.
func (w *Writer) RecordTurn(userText, replyText, cwd string) {
	w.mu.Lock()
	w.lastTurnAt = w.now()
	w.mu.Unlock()

	select {
	case w.ch <- turnMemo{userText: userText, replyText: replyText, cwd: cwd}:
	default:
		if w.log != nil {
			w.log.Warnf("butler-memory: queue full, dropping turn (cwd=%q)", cwd)
		}
	}
}

func (w *Writer) run() {
	if w.consolidator != nil && w.tickInterval > 0 {
		go w.tickLoop()
	}
	for {
		select {
		case memo, ok := <-w.ch:
			if !ok {
				return
			}
			w.process(memo)
			// Threshold trigger: the inbox just grew; check if it crossed 75%.
			w.maybeConsolidate()
		case <-w.consolidateCh:
			// Idle / backstop trigger fired by the ticker.
			w.maybeConsolidate()
		}
	}
}

// tickLoop periodically nudges the worker to evaluate the time-based triggers.
// The nudge is non-blocking (满即丢): if a consolidation is already queued or in
// flight, dropping the tick is harmless — the next tick re-evaluates.
func (w *Writer) tickLoop() {
	t := time.NewTicker(w.tickInterval)
	defer t.Stop()
	for range t.C {
		select {
		case w.consolidateCh <- struct{}{}:
		default:
		}
	}
}

// maybeConsolidate evaluates the trigger and, when it fires, runs one
// consolidation pass on the worker. All failures are swallowed; the inbox is
// only cleared inside Consolidate on full success.
func (w *Writer) maybeConsolidate() {
	if w.consolidator == nil {
		return
	}
	bytes, has := w.consolidator.inboxBytes()

	w.mu.Lock()
	now := w.now()
	state := triggerState{
		now:               now,
		inboxBytes:        bytes,
		hasInbox:          has,
		lastTurnAt:        w.lastTurnAt,
		lastConsolidateAt: w.lastConsolidateAt,
	}
	w.mu.Unlock()

	fire, reason := decideTrigger(state, w.trig)
	if !fire {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), consolidateTimeout)
	defer cancel()
	if err := w.consolidator.Consolidate(ctx); err != nil {
		if w.log != nil {
			w.log.Warnf("butler-memory: consolidate failed (trigger=%s): %v", reason, err)
		}
		return
	}

	w.mu.Lock()
	w.lastConsolidateAt = now
	w.mu.Unlock()
	if w.log != nil {
		w.log.Infof("butler-memory: consolidation done (trigger=%s)", reason)
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
