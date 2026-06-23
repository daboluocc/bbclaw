package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// blockingDriver simulates a long-running turn: Send emits one text frame
// then blocks until Interrupt is called, after which it emits EvInterrupted +
// EvTurnEnd — mirroring the claudecode driver's barge-in contract
// (ADR-028 §2.5.1).
type blockingDriver struct {
	mu        sync.Mutex
	events    chan agent.Event
	received  []string
	interrupt chan struct{}
}

func newBlockingDriver() *blockingDriver {
	return &blockingDriver{
		events:    make(chan agent.Event, 64),
		interrupt: make(chan struct{}, 1),
	}
}

func (b *blockingDriver) Name() string                     { return "blocking" }
func (b *blockingDriver) Capabilities() agent.Capabilities { return agent.Capabilities{Streaming: true} }

func (b *blockingDriver) Start(_ context.Context, _ agent.StartOpts) (agent.SessionID, error) {
	return "blocking-sid", nil
}

func (b *blockingDriver) Send(_ agent.SessionID, text string) error {
	b.mu.Lock()
	b.received = append(b.received, text)
	b.mu.Unlock()
	b.events <- agent.Event{Type: agent.EvText, Text: "partial reply"}
	select {
	case <-b.interrupt:
		b.events <- agent.Event{Type: agent.EvInterrupted}
	case <-time.After(10 * time.Second):
		// Safety valve so a broken test doesn't hang the suite.
	}
	b.events <- agent.Event{Type: agent.EvTurnEnd}
	return nil
}

func (b *blockingDriver) Events(_ agent.SessionID) <-chan agent.Event { return b.events }
func (b *blockingDriver) Approve(_ agent.SessionID, _ agent.ToolID, _ agent.Decision) error {
	return agent.ErrUnsupported
}
func (b *blockingDriver) Stop(_ agent.SessionID) error { return nil }

// Interrupt implements agent.Interrupter.
func (b *blockingDriver) Interrupt(_ agent.SessionID) error {
	select {
	case b.interrupt <- struct{}{}:
	default:
	}
	return nil
}

func (b *blockingDriver) receivedTexts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.received))
	copy(out, b.received)
	return out
}

// TestHandleAgentCancel_EndToEnd drives the full barge-in chain over HTTP:
// an in-flight /v1/agent/message turn is aborted by POST /v1/agent/cancel,
// the NDJSON stream ends with turn_cancelled + turn_end, and the NEXT turn's
// prompt is the user's CLEAN text — per ADR-028 §2.5.1 (撤回语义) the interrupt
// is "withdrawn, as if it never happened", so NO interruption note is injected.
func TestHandleAgentCancel_EndToEnd(t *testing.T) {
	drv := newBlockingDriver()
	srv := NewServer(AppConfig{}, nil, nil, nil, nil, obs.NewLogger(), obs.NewMetrics())
	srv.SetAgentDriver(drv)
	srv.SetInflight(butler.NewInflightRegistry())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Kick off the turn; collect frame types from the NDJSON stream.
	frames := make(chan string, 32)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		resp, err := http.Post(ts.URL+"/v1/agent/message?deviceId=dev-t", "application/json",
			strings.NewReader(`{"text":"长任务"}`))
		if err != nil {
			t.Errorf("POST message: %v", err)
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var frame map[string]any
			if json.Unmarshal(sc.Bytes(), &frame) == nil {
				if typ, _ := frame["type"].(string); typ != "" {
					frames <- typ
				}
			}
		}
	}()

	// Wait until the first text frame proves the turn is in flight.
	select {
	case typ := <-frames:
		if typ != "session" && typ != "text" {
			t.Logf("first frame: %s", typ)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first frame")
	}

	// Barge-in.
	resp, err := http.Post(ts.URL+"/v1/agent/cancel", "application/json",
		strings.NewReader(`{"deviceId":"dev-t","playedText":"今天天气晴。","playedSeq":2}`))
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	var cancelResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Cancelled bool `json:"cancelled"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cancelResp); err != nil {
		t.Fatalf("decode cancel resp: %v", err)
	}
	resp.Body.Close()
	if !cancelResp.OK || !cancelResp.Data.Cancelled {
		t.Fatalf("cancel: want ok+cancelled, got %+v", cancelResp)
	}

	// The stream must finish promptly with turn_cancelled then turn_end.
	var sawCancelled, sawTurnEnd bool
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case typ, ok := <-frames:
			if !ok {
				break collect
			}
			switch typ {
			case "turn_cancelled":
				sawCancelled = true
			case "turn_end":
				sawTurnEnd = true
			}
		case <-streamDone:
			// Drain whatever is left.
			for {
				select {
				case typ := <-frames:
					switch typ {
					case "turn_cancelled":
						sawCancelled = true
					case "turn_end":
						sawTurnEnd = true
					}
				default:
					break collect
				}
			}
		case <-deadline:
			t.Fatal("stream did not finish after cancel")
		}
	}
	if !sawCancelled {
		t.Error("want turn_cancelled frame in stream")
	}
	if !sawTurnEnd {
		t.Error("want turn_end frame in stream")
	}

	// Next turn: prompt must carry the interruption note + original text.
	resp2, err := http.Post(ts.URL+"/v1/agent/message?deviceId=dev-t", "application/json",
		strings.NewReader(`{"text":"继续"}`))
	if err != nil {
		t.Fatalf("POST second message: %v", err)
	}
	// Unblock the driver so the second turn completes.
	_ = drv.Interrupt("blocking-sid")
	_, _ = bufio.NewReader(resp2.Body).ReadString(0)
	resp2.Body.Close()

	texts := drv.receivedTexts()
	if len(texts) != 2 {
		t.Fatalf("want 2 sends, got %d: %v", len(texts), texts)
	}
	// ADR-028 §2.5.1 修订(撤回语义):打断 = 当没发生过。两轮都必须是用户的
	// 原始文本,第二轮不得携带任何"打断/已播内容"备注。
	if strings.Contains(texts[0], "打断") {
		t.Errorf("first turn must be clean user text, got %q", texts[0])
	}
	if texts[1] != "继续" {
		t.Errorf("second turn must be the clean user text with no injected note, got %q", texts[1])
	}
}
