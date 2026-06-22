//go:build e2e

// Cloud-relay acceptance gate: a mock BBClaw Cloud sends a voice.transcript, and
// adapter_v2 — registered as a HomeAdapter — must inject it into a real PTY
// (cmd/mockcli), stream voice.reply.delta, and finish with voice.reply carrying
// the scraped answer. Proves the SaaS path end to end with no real cloud and no
// device, using the same wire frames the deployed cloud uses.
//
// Run via `make -C adapter_v2 e2e` (build tag e2e).
package cloudrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestE2ECloudRelayVoiceTurn(t *testing.T) {
	bin := buildMockCLI(t)

	// Channels for the mock cloud to report what adapter_v2 sent back.
	type result struct {
		sawInfo  bool
		deltas   int
		replyOK  bool
		replyTxt string
	}
	resCh := make(chan result, 1)

	// Mock cloud: accept the home-adapter WS, send welcome + voice.transcript,
	// then collect the adapter's frames until voice.reply.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("role") != "home_adapter" || r.URL.Query().Get("home_site_id") == "" {
			http.Error(w, "bad role/site", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()

		_ = wsjson.Write(ctx, c, Envelope{Type: "welcome", Payload: map[string]any{"homeAdapterRegistration": "claimed"}})
		_ = wsjson.Write(ctx, c, Envelope{
			Type: "request", MessageID: "m1", DeviceID: "dev-1", Kind: "voice.transcript",
			Payload: map[string]any{"text": "hello", "sessionKey": "s1", "streamId": "st1"},
		})

		var res result
		for {
			var env Envelope
			if err := wsjson.Read(ctx, c, &env); err != nil {
				break
			}
			switch {
			case env.Type == "info":
				res.sawInfo = true
			case env.Type == "event" && env.Kind == "voice.reply.delta":
				res.deltas++
			case env.Type == "reply" && env.Kind == "voice.reply":
				res.replyOK, _ = env.Payload["ok"].(bool)
				res.replyTxt, _ = env.Payload["text"].(string)
				resCh <- res
				return
			}
		}
		resCh <- res
	}))
	defer srv.Close()

	mgr := session.NewManager()
	relay := New(mgr, butler.NewDeviceSession(mgr, []string{bin}, ""), Config{
		CloudWSURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		HomeSiteID:     "11111111-1111-1111-1111-111111111111",
		ReconnectDelay: time.Second,
		ReplyWait:      20 * time.Second,
	}, func(string, ...any) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relay.Run(ctx)

	select {
	case res := <-resCh:
		if !res.sawInfo {
			t.Error("adapter never sent the info frame after welcome")
		}
		if !res.replyOK {
			t.Errorf("voice.reply ok=false")
		}
		if !strings.Contains(res.replyTxt, "ANSWER: hello") {
			t.Errorf("voice.reply text = %q, want a substring \"ANSWER: hello\"", res.replyTxt)
		}
		if res.deltas == 0 {
			t.Errorf("no voice.reply.delta streamed (StreamReplyDelta on)")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("timed out waiting for voice.reply")
	}
}

// buildMockCLI compiles cmd/mockcli into a temp dir and returns the path.
func buildMockCLI(t *testing.T) string {
	t.Helper()
	goBin := goToolPath()
	bin := filepath.Join(t.TempDir(), "mockcli")
	cmd := exec.Command(goBin, "build", "-o", bin, "./cmd/mockcli")
	cmd.Dir = moduleRoot(t, goBin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockcli: %v\n%s", err, out)
	}
	return bin
}

func moduleRoot(t *testing.T, goBin string) string {
	t.Helper()
	out, err := exec.Command(goBin, "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		t.Fatalf("no go.mod from %s", goBin)
	}
	return filepath.Dir(gomod)
}

func goToolPath() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	for _, c := range []string{"/opt/homebrew/bin/go", "/usr/local/go/bin/go", "/usr/local/bin/go"} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "go"
}
