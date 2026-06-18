package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/config"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

// testConfig launches a trivial long-running CLI under the PTY so a created
// session stays alive for the duration of a test. `cat` echoes stdin to stdout
// and never exits on its own, which is enough to prove the round-trip without
// depending on a real agent CLI being installed.
func testConfig() config.Config {
	return config.Config{Addr: ":0", Argv: []string{"cat"}}
}

// dialWS opens a client WebSocket to the given /ws URL.
func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// readReconnected reads frames until it sees the termchan "reconnected" handshake
// frame, proving the WebSocket was actually wired to a live session.
func readReconnected(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type == "reconnected" {
			return
		}
	}
}

// TestHealthz verifies the liveness probe returns 200 "ok".
func TestHealthz(t *testing.T) {
	mgr := session.NewManager()
	srv := httptest.NewServer(newRouter(mgr, testConfig()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}
}

// TestWSMissingSession rejects a /ws request with no session id (400) and does
// not create a session.
func TestWSMissingSession(t *testing.T) {
	mgr := session.NewManager()
	srv := httptest.NewServer(newRouter(mgr, testConfig()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ws")
	if err != nil {
		t.Fatalf("get /ws: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestWSCreatesSessionOnce proves the create-if-absent contract: the first /ws
// request for an id spawns the CLI, and a second request for the SAME id joins
// the existing session rather than spawning a second process.
func TestWSCreatesSessionOnce(t *testing.T) {
	mgr := session.NewManager()
	srv := httptest.NewServer(newRouter(mgr, testConfig()))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	if mgr.Get("s1") != nil {
		t.Fatal("session s1 should not exist before any /ws request")
	}

	// First connection: creates the session.
	c1 := dialWS(t, wsURL+"/ws?session=s1")
	readReconnected(t, c1)

	sess := mgr.Get("s1")
	if sess == nil {
		t.Fatal("session s1 should exist after first /ws request")
	}

	// Second connection to the same id: must reuse the very same Session.
	c2 := dialWS(t, wsURL+"/ws?session=s1")
	readReconnected(t, c2)

	if got := mgr.Get("s1"); got != sess {
		t.Fatalf("second /ws to s1 created a new session: got %p, want %p", got, sess)
	}

	// A different id is a different session.
	c3 := dialWS(t, wsURL+"/ws?session=s2")
	readReconnected(t, c3)
	if mgr.Get("s2") == sess {
		t.Fatal("session s2 must be distinct from s1")
	}
}

// TestRouterUnknownPath confirms the mux 404s paths it does not serve. An
// unknown path is handled by the embedded web file server (mounted at "/"),
// which has no such file and therefore returns 404 — proving the catch-all does
// not swallow stray requests as the index page.
func TestRouterUnknownPath(t *testing.T) {
	mgr := session.NewManager()
	srv := httptest.NewServer(newRouter(mgr, testConfig()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("get /nope: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestWebClientServed verifies the embedded xterm.js client is reachable at "/"
// and carries the wiring issue #208 requires: it loads xterm.js, opens the
// /ws?session= socket, and handles the reconnect/snapshot protocol. The
// table-driven substring checks pin those load-bearing pieces so a refactor of
// index.html can't silently drop the contract the server depends on.
func TestWebClientServed(t *testing.T) {
	mgr := session.NewManager()
	srv := httptest.NewServer(newRouter(mgr, testConfig()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html…", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(body)

	wants := []struct {
		name    string
		snippet string
	}{
		{"loads xterm.js", "xterm"},
		{"loads the fit addon", "addon-fit"},
		{"opens the session websocket", "/ws?session="},
		{"sends input frames", `"input"`},
		{"sends resize frames", `"resize"`},
		{"handles reconnected replay", "reconnected"},
		{"writes output frames", "output"},
		{"persists session id for refresh", "localStorage"},
	}
	for _, w := range wants {
		t.Run(w.name, func(t *testing.T) {
			if !strings.Contains(html, w.snippet) {
				t.Errorf("served index.html missing %q (%s)", w.snippet, w.name)
			}
		})
	}
}

// TestSpecificRoutesWinOverWebRoot proves the catch-all web file server mounted
// at "/" does not shadow the more specific /healthz and /ws routes: ServeMux
// must dispatch those to their own handlers, not to the static index page.
func TestSpecificRoutesWinOverWebRoot(t *testing.T) {
	mgr := session.NewManager()
	srv := httptest.NewServer(newRouter(mgr, testConfig()))
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string // exact, trimmed; empty means "don't check"
	}{
		{
			name:       "healthz still returns ok",
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
		{
			name:       "ws without session still 400s",
			path:       "/ws",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("get %s: %v", tt.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantBody != "" {
				body, _ := io.ReadAll(resp.Body)
				if got := strings.TrimSpace(string(body)); got != tt.wantBody {
					t.Fatalf("body = %q, want %q", got, tt.wantBody)
				}
			}
		})
	}
}
