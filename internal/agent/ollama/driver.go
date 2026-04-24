// Package ollama implements agent.Driver on top of an Ollama HTTP server
// (https://github.com/ollama/ollama). The driver keeps the conversation
// history in-memory per session and POSTs to /api/chat with stream=true
// for each Send.
//
// Capabilities advertised:
//   - ToolApproval: false (Ollama has no tool-use prompts)
//   - Resume: true (we keep history in-process; same SessionID = same history)
//   - Streaming: true
//   - MaxInputBytes: 65536
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

const (
	driverName      = "ollama"
	defaultBase     = "http://127.0.0.1:11434"
	defaultModel    = "llama3.2"
	eventBufSize    = 64
	maxHistoryPairs = 50 // cap on total messages in session.messages
	requestTimeout  = 0  // 0 = no global timeout; chat streams can be long
)

// Options configures the driver.
type Options struct {
	// BaseURL is the Ollama HTTP endpoint root (e.g. http://127.0.0.1:11434).
	// Empty falls back to OLLAMA_BASE_URL env, then defaultBase.
	BaseURL string
	// Model is the ollama model tag (e.g. "llama3.2", "qwen2.5:7b"). Empty
	// falls back to OLLAMA_MODEL env, then defaultModel.
	Model string
	// HTTPClient is used for chat requests. Nil = http.DefaultClient. Tests
	// inject one pointed at httptest.Server.
	HTTPClient *http.Client
}

// Driver is the Ollama AgentDriver implementation.
type Driver struct {
	baseURL string
	model   string
	http    *http.Client
	log     *obs.Logger

	mu       sync.Mutex
	sessions map[agent.SessionID]*session
}

// New constructs a Driver. The logger is required.
//
// Endpoint is hardcoded to 127.0.0.1:11434 — if you run Ollama somewhere
// else, pass Options.BaseURL explicitly from your wiring code. We don't
// expose a OLLAMA_BASE_URL env on principle: 99% of operators run Ollama
// on localhost, and an env knob for the 1% edge case just clutters .env.
func New(opts Options, log *obs.Logger) *Driver {
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = defaultBase
	}
	base = strings.TrimRight(base, "/")

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OLLAMA_MODEL"))
	}
	if model == "" {
		model = defaultModel
	}

	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	return &Driver{
		baseURL:  base,
		model:    model,
		http:     hc,
		log:      log,
		sessions: make(map[agent.SessionID]*session),
	}
}

// Name implements agent.Driver.
func (d *Driver) Name() string { return driverName }

// Capabilities implements agent.Driver.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		ToolApproval:  false,
		Resume:        true,
		Streaming:     true,
		MaxInputBytes: 64 * 1024,
	}
}

// Start allocates a new session. No HTTP is performed here; history is
// built up in Send.
func (d *Driver) Start(ctx context.Context, opts agent.StartOpts) (agent.SessionID, error) {
	sid := agent.SessionID("ol-" + uuid.NewString())
	s := &session{
		id:       sid,
		events:   make(chan agent.Event, eventBufSize),
		rootCtx:  ctx,
		messages: nil,
	}
	d.mu.Lock()
	d.sessions[sid] = s
	d.mu.Unlock()
	return sid, nil
}

// Events implements agent.Driver.
func (d *Driver) Events(sid agent.SessionID) <-chan agent.Event {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		ch := make(chan agent.Event)
		close(ch)
		return ch
	}
	return s.events
}

// Send appends the user message to the session history, POSTs to /api/chat
// in streaming mode, emits EvText / EvTokens / EvTurnEnd, and appends the
// assistant reply back into the session history. Blocks until the stream
// completes.
func (d *Driver) Send(sid agent.SessionID, text string) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}

	s.appendMessage(ollamaMessage{Role: "user", Content: text})

	reqBody := ollamaChatRequest{
		Model:    d.model,
		Messages: s.snapshotMessages(),
		Stream:   true,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("ollama: marshal request: %w", err)
	}

	ctx, cancel := context.WithCancel(s.rootCtx)
	defer cancel()
	s.setCancel(cancel)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", d.baseURL+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	tStart := time.Now()
	resp, err := d.http.Do(httpReq)
	if err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("ollama chat: %v", err)})
		s.emit(agent.Event{Type: agent.EvTurnEnd})
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read a bounded chunk of the body so we don't spam logs on a huge
		// HTML error page.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		s.emit(agent.Event{
			Type: agent.EvError,
			Text: fmt.Sprintf("ollama chat: http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet))),
		})
		s.emit(agent.Event{Type: agent.EvTurnEnd})
		return nil
	}

	d.log.Infof("ollama: chat sid=%s model=%s history_len=%d", sid, d.model, len(reqBody.Messages))

	fullReply, emittedEnd := parseStream(resp.Body, s, d.log)

	// Record the assistant reply in session history, unless the stream
	// produced nothing (e.g. upstream error mid-stream).
	if fullReply != "" {
		s.appendMessage(ollamaMessage{Role: "assistant", Content: fullReply})
	}

	if !emittedEnd {
		s.emit(agent.Event{Type: agent.EvTurnEnd})
	}
	d.log.Infof("ollama: chat done sid=%s elapsed_ms=%d reply_bytes=%d",
		sid, time.Since(tStart).Milliseconds(), len(fullReply))
	return nil
}

// Approve is not supported — Ollama has no tool-use prompts.
func (d *Driver) Approve(sid agent.SessionID, tid agent.ToolID, decision agent.Decision) error {
	return agent.ErrUnsupported
}

// Stop terminates any in-flight request and closes the session.
func (d *Driver) Stop(sid agent.SessionID) error {
	d.mu.Lock()
	s, ok := d.sessions[sid]
	delete(d.sessions, sid)
	d.mu.Unlock()
	if !ok {
		return agent.ErrUnknownSession
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if !closed {
		close(s.events)
	}
	return nil
}

// ─── session ────────────────────────────────────────────────────────────

type session struct {
	id      agent.SessionID
	events  chan agent.Event
	rootCtx context.Context

	seq uint64

	mu       sync.Mutex
	cancel   context.CancelFunc
	messages []ollamaMessage
	closed   bool
}

func (s *session) setCancel(c context.CancelFunc) {
	s.mu.Lock()
	s.cancel = c
	s.mu.Unlock()
}

func (s *session) emit(e agent.Event) {
	e.Seq = atomic.AddUint64(&s.seq, 1)
	select {
	case s.events <- e:
	case <-s.rootCtx.Done():
	}
}

// appendMessage adds m to the history, enforcing the maxHistoryPairs cap.
// If a system message ever gets prepended (role=="system"), we preserve it
// when trimming. Current callers never inject a system message; the guard
// is forward-compatible.
func (s *session) appendMessage(m ollamaMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	if len(s.messages) <= maxHistoryPairs {
		return
	}
	// Preserve a leading system message if present.
	excess := len(s.messages) - maxHistoryPairs
	if len(s.messages) > 0 && s.messages[0].Role == "system" {
		// Keep messages[0]; drop from index 1..1+excess.
		tail := append([]ollamaMessage{s.messages[0]}, s.messages[1+excess:]...)
		s.messages = tail
		return
	}
	s.messages = append([]ollamaMessage(nil), s.messages[excess:]...)
}

func (s *session) snapshotMessages() []ollamaMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ollamaMessage, len(s.messages))
	copy(out, s.messages)
	return out
}

// ─── wire types ─────────────────────────────────────────────────────────

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// ollamaChatChunk matches the NDJSON schema /api/chat emits:
//
//	{"model":"...","created_at":"...","message":{"role":"assistant","content":"..."},"done":false}
//	{"model":"...","created_at":"...","done":true,"total_duration":...,"eval_count":N,"prompt_eval_count":M}
type ollamaChatChunk struct {
	Model           string         `json:"model"`
	Message         *ollamaMessage `json:"message,omitempty"`
	Done            bool           `json:"done"`
	EvalCount       int            `json:"eval_count,omitempty"`
	PromptEvalCount int            `json:"prompt_eval_count,omitempty"`
	Error           string         `json:"error,omitempty"`
}

// parseStream reads the NDJSON body, emits events, and returns the
// concatenated assistant reply plus whether EvTurnEnd was already emitted.
func parseStream(r io.Reader, s *session, log *obs.Logger) (string, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var reply strings.Builder
	turnEnded := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			log.Warnf("ollama: unparseable chunk sid=%s err=%v line=%q", s.id, err, truncate(string(line), 200))
			continue
		}
		if chunk.Error != "" {
			s.emit(agent.Event{Type: agent.EvError, Text: "ollama: " + chunk.Error})
			continue
		}
		if chunk.Message != nil && chunk.Message.Content != "" {
			reply.WriteString(chunk.Message.Content)
			s.emit(agent.Event{Type: agent.EvText, Text: chunk.Message.Content})
		}
		if chunk.Done {
			if chunk.EvalCount > 0 || chunk.PromptEvalCount > 0 {
				s.emit(agent.Event{
					Type: agent.EvTokens,
					Tokens: &agent.Tokens{
						In:  chunk.PromptEvalCount,
						Out: chunk.EvalCount,
					},
				})
			}
			s.emit(agent.Event{Type: agent.EvTurnEnd})
			turnEnded = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		s.emit(agent.Event{Type: agent.EvError, Text: fmt.Sprintf("ollama stream read: %v", err)})
	}
	return reply.String(), turnEnded
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
