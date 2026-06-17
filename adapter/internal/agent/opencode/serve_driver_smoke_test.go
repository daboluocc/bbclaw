package opencode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// TestServeDriverSmoke drives the real serve+SDK path against a live
// `opencode serve`. It is gated on OC_SMOKE=1 (and a working model) so it never
// runs in CI without opencode + provider creds.
//
//	OC_SMOKE=1 OC_MODEL=deepseek/deepseek-v4-pro go test ./internal/agent/opencode/ -run Smoke -v
func TestServeDriverSmoke(t *testing.T) {
	if os.Getenv("OC_SMOKE") == "" {
		t.Skip("set OC_SMOKE=1 (and OC_MODEL=provider/model) to run the live serve smoke test")
	}
	model := os.Getenv("OC_MODEL")

	d := NewServe(Options{}, obs.NewLogger())
	defer d.Shutdown()

	caps := d.Capabilities()
	if !caps.Streaming || !caps.Resume {
		t.Fatalf("unexpected caps: %+v", caps)
	}

	ctx := context.Background()
	cwd, _ := os.MkdirTemp("", "oc-smoke-*")
	sid, err := d.Start(ctx, agent.StartOpts{
		Cwd:          cwd,
		Model:        model,
		SystemPrompt: "You are a terse test fixture.",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("serve version=%s session=%s", d.serve.currentVersion(), sid)

	events := d.Events(sid)
	var text strings.Builder
	gotTurnEnd := false

	go func() {
		if err := d.Send(sid, "Reply with exactly: SMOKE-OK"); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()

	timeout := time.After(75 * time.Second)
	for !gotTurnEnd {
		select {
		case ev := <-events:
			switch ev.Type {
			case agent.EvText:
				text.WriteString(ev.Text)
			case agent.EvTokens:
				if ev.Tokens != nil {
					t.Logf("tokens in=%d out=%d", ev.Tokens.In, ev.Tokens.Out)
				}
			case agent.EvError:
				t.Logf("EvError: %s", ev.Text)
			case agent.EvTurnEnd:
				gotTurnEnd = true
			}
		case <-timeout:
			t.Fatalf("timed out; text so far=%q", text.String())
		}
	}

	got := strings.TrimSpace(text.String())
	t.Logf("assistant text=%q", got)
	if got == "" {
		t.Fatalf("no streaming text received")
	}

	// Exercise ModelLister + SessionLister against the same live serve.
	if models, err := d.ListModels(ctx); err != nil {
		t.Errorf("ListModels: %v", err)
	} else {
		t.Logf("ListModels returned %d models (e.g. %v)", len(models), firstN(models, 3))
	}
	if sessions, err := d.ListSessions(ctx, 5); err != nil {
		t.Errorf("ListSessions: %v", err)
	} else if len(sessions) == 0 {
		t.Errorf("ListSessions returned 0 — expected the session we just created")
	} else {
		t.Logf("ListSessions returned %d (newest preview=%q)", len(sessions), sessions[0].Preview)
	}

	if err := d.Stop(sid); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// TestServeMCPRegistrationSmoke proves the butler dispatch wiring: registering
// an MCP server via POST /mcp succeeds, GET /mcp lists it, and the implied
// dispatch tool name is recognized. (The model actually calling the tool needs
// a tool-reliable model + the real butlermcp server, validated separately.)
func TestServeMCPRegistrationSmoke(t *testing.T) {
	if os.Getenv("OC_SMOKE") == "" {
		t.Skip("set OC_SMOKE=1 to run the live MCP-registration smoke test")
	}
	d := NewServe(Options{}, obs.NewLogger())
	defer d.Shutdown()

	ctx := context.Background()
	specs := []agent.MCPServerSpec{{
		Name:    "bbclaw",
		Command: "true", // benign: registration records config; handshake may fail, that's fine
		Args:    []string{"mcp-server"},
		Env:     map[string]string{"BBCLAW_TEST": "1"},
	}}
	// POST /mcp accepted (the dummy command will report a failed connection in
	// the response, but registration itself succeeds — the real butlermcp server
	// connects). 200 → registerMCPServers returns nil.
	if err := d.registerMCPServers(ctx, specs); err != nil {
		t.Fatalf("registerMCPServers: %v", err)
	}
	if !d.isDispatchTool("bbclaw_dispatch") {
		t.Errorf("isDispatchTool(bbclaw_dispatch) = false after registration")
	}
	if !d.isDispatchTool("bbclaw*dispatch") {
		t.Errorf("lenient isDispatchTool(bbclaw*dispatch) = false after registration")
	}

	// Idempotent: a second registration is a no-op (no duplicate POST).
	if err := d.registerMCPServers(ctx, specs); err != nil {
		t.Errorf("second registerMCPServers: %v", err)
	}
	t.Logf("MCP registration accepted; dispatch tool 'bbclaw_dispatch' recognized")
}

func firstN(m []agent.ModelInfo, n int) []string {
	var out []string
	for i := 0; i < len(m) && i < n; i++ {
		out = append(out, m[i].ID)
	}
	return out
}
