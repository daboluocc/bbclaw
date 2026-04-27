package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent/claudecode"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// Integration smoke: actually spawn `claude -p ...` and drain the NDJSON
// stream through the HTTP handler. Requires the `claude` CLI on PATH and a
// logged-in session; skipped otherwise so unit-test runs stay hermetic.
//
//	BBCLAW_AGENT_INTEGRATION=1 go test -run TestAgentMessageIntegration \
//	    -v -timeout 120s ./internal/httpapi/...
func TestAgentMessageIntegration(t *testing.T) {
	if os.Getenv("BBCLAW_AGENT_INTEGRATION") != "1" {
		t.Skip("set BBCLAW_AGENT_INTEGRATION=1 to run (requires `claude` CLI + login)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude CLI not on PATH: %v", err)
	}

	logger := obs.NewLogger()
	metrics := obs.NewMetrics()

	srv := NewServer(
		AppConfig{AuthToken: ""}, // no auth, matches user's .env
		nil, nil, nil, nil,
		logger, metrics,
	)
	srv.SetAgentDriver(claudecode.New(claudecode.Options{}, logger))

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	body := bytes.NewBufferString(`{"text":"Reply with exactly these five words: hello from bbclaw adapter smoke"}`)
	req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/agent/message", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("content-type=%q, want application/x-ndjson", ct)
	}

	var (
		gotText    bool
		gotTurnEnd bool
		fullText   strings.Builder
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		t.Logf("frame: %s", line)
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Errorf("unparseable frame: %v line=%s", err, line)
			continue
		}
		switch frame["type"] {
		case "text":
			gotText = true
			if s, ok := frame["text"].(string); ok {
				fullText.WriteString(s)
			}
		case "turn_end":
			gotTurnEnd = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner err: %v", err)
	}

	if !gotText {
		t.Error("expected at least one text frame")
	}
	if !gotTurnEnd {
		t.Error("expected a turn_end frame")
	}
	t.Logf("assembled assistant text: %q", fullText.String())
}
