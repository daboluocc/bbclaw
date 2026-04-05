package openclaw

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSendSlashCommandWS tests sending /status via WS chat.send to a live Gateway.
// Requires OPENCLAW_WS_URL and OPENCLAW_AUTH_TOKEN env vars.
// Run: OPENCLAW_WS_URL=ws://127.0.0.1:18789 OPENCLAW_AUTH_TOKEN=<token> go test -run TestSendSlashCommandWS -v
func TestSendSlashCommandWS(t *testing.T) {
	wsURL := os.Getenv("OPENCLAW_WS_URL")
	token := os.Getenv("OPENCLAW_AUTH_TOKEN")
	if wsURL == "" || token == "" {
		t.Skip("OPENCLAW_WS_URL and OPENCLAW_AUTH_TOKEN required")
	}

	identityPath := ""
	if p := os.Getenv("OPENCLAW_IDENTITY_PATH"); p != "" {
		identityPath = p
	}

	client := NewClient(wsURL, 30*time.Second, Options{
		AuthToken:          token,
		DeviceIdentityPath: identityPath,
		NodeID:             "bbclaw-test",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reply, err := client.SendSlashCommand(ctx, "/status", "agent:main:bbclaw")
	if err != nil {
		t.Fatalf("SendSlashCommand failed: %v", err)
	}
	t.Logf("reply: %s", reply)

	// /status should return something with model info or session info
	if reply == "" {
		t.Log("warning: empty reply (command may have been executed without text response)")
	}
}

// TestSendSlashCommandNew tests /new (reset session).
func TestSendSlashCommandNew(t *testing.T) {
	wsURL := os.Getenv("OPENCLAW_WS_URL")
	token := os.Getenv("OPENCLAW_AUTH_TOKEN")
	if wsURL == "" || token == "" {
		t.Skip("OPENCLAW_WS_URL and OPENCLAW_AUTH_TOKEN required")
	}

	client := NewClient(wsURL, 30*time.Second, Options{
		AuthToken:          token,
		DeviceIdentityPath: os.Getenv("OPENCLAW_IDENTITY_PATH"),
		NodeID:             "bbclaw-test",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reply, err := client.SendSlashCommand(ctx, "/new", "agent:main:bbclaw")
	if err != nil {
		t.Fatalf("SendSlashCommand /new failed: %v", err)
	}
	t.Logf("/new reply: %q", reply)
}

// TestSendSlashCommandStop tests /stop.
func TestSendSlashCommandStop(t *testing.T) {
	wsURL := os.Getenv("OPENCLAW_WS_URL")
	token := os.Getenv("OPENCLAW_AUTH_TOKEN")
	if wsURL == "" || token == "" {
		t.Skip("OPENCLAW_WS_URL and OPENCLAW_AUTH_TOKEN required")
	}

	client := NewClient(wsURL, 30*time.Second, Options{
		AuthToken:          token,
		DeviceIdentityPath: os.Getenv("OPENCLAW_IDENTITY_PATH"),
		NodeID:             "bbclaw-test",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reply, err := client.SendSlashCommand(ctx, "/stop", "agent:main:bbclaw")
	if err != nil {
		// /stop may return error if nothing is running — that's OK
		if strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("SendSlashCommand /stop unauthorized: %v", err)
		}
		t.Logf("/stop returned error (may be expected): %v", err)
		return
	}
	t.Logf("/stop reply: %q", reply)
}
