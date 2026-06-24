// Package cloudrelay makes adapter_v2 register with the BBClaw Cloud as a
// HomeAdapter, so the device can select it in its SaaS site picker (sites.list)
// and have voice relayed to it — exactly like v1, with ZERO cloud or firmware
// changes (the cloud is adapter-type-agnostic; it keys peers only by role +
// home_site_id and never checks a version or capability).
//
// In cloud_saas the cloud does ASR/TTS and relays only TEXT: it sends a
// voice.transcript request, adapter_v2 injects the transcript into a per-device
// PTY (deviceapi.Bridge — the same PTY inject + extract validated against real
// claude), streams the reply back as voice.reply.delta events, and ends with a
// voice.reply. No audio, no ASR/TTS on this path (that is the bbwire/2 LAN-direct
// line's job).
//
// The wire protocol (Envelope, the welcome→info handshake, registration.code,
// voice.transcript / voice.reply.delta / voice.reply / heartbeat) is ported from
// v1's internal/homeadapter so it is byte-compatible with the deployed cloud.
package cloudrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/buildinfo"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/butler"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/curdevice"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
	"github.com/google/uuid"
)

// appPingInterval is the application-level JSON ping cadence. WebSocket control
// PINGs can be absorbed by an nginx front, so the cloud's ~35s read deadline is
// kept alive with a JSON {"type":"ping"} (matches v1).
const appPingInterval = 25 * time.Second

// Envelope is the JSON frame exchanged with the cloud over the one home-adapter
// WebSocket. Ported verbatim from v1 (internal/homeadapter CloudEnvelope) so the
// cloud treats adapter_v2 as an identical peer.
type Envelope struct {
	Type       string         `json:"type"` // ping|welcome|ack|event|request|reply|info
	MessageID  string         `json:"messageId,omitempty"`
	DeviceID   string         `json:"deviceId,omitempty"`
	HomeSiteID string         `json:"homeSiteId,omitempty"`
	SessionID  string         `json:"sessionId,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// Config is the cloud-relay configuration, read from the same env vars as v1.
type Config struct {
	CloudWSURL     string        // CLOUD_WS_URL (default wss://bbclaw.daboluo.cc/ws)
	CloudAuthToken string        // CLOUD_AUTH_TOKEN (optional; anon home-adapter allowed)
	HomeSiteID     string        // stable UUID identifying this adapter as a site
	ReconnectDelay time.Duration // backoff between dial attempts
	ReplyWait      time.Duration // max time to wait for a turn's reply
}

// Enabled reports whether the cloud relay should run: only when CLOUD_WS_URL is
// explicitly set (so a bare `make run` stays LAN-only and never dials the cloud).
func Enabled() bool {
	return strings.TrimSpace(os.Getenv("CLOUD_WS_URL")) != ""
}

// LoadConfig builds the relay config from the environment, ensuring a stable
// home_site_id (persisted, so the claimed binding survives restarts).
func LoadConfig() (Config, error) {
	siteID, err := ensureHomeSiteID()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		CloudWSURL:     getOrDefault("CLOUD_WS_URL", "wss://bbclaw.daboluo.cc/ws"),
		CloudAuthToken: strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")),
		HomeSiteID:     siteID,
		ReconnectDelay: 3 * time.Second,
		ReplyWait:      90 * time.Second,
	}
	return cfg, nil
}

// dialURL builds wss://host/ws?role=home_adapter&home_site_id=…&token=… from the
// base URL, normalising scheme and forcing the /ws path (ported from v1).
func (c Config) dialURL() string {
	base := strings.TrimSpace(c.CloudWSURL)
	scheme := "wss://"
	rest := base
	switch {
	case strings.HasPrefix(base, "https://"):
		scheme, rest = "wss://", base[len("https://"):]
	case strings.HasPrefix(base, "http://"):
		scheme, rest = "ws://", base[len("http://"):]
	case strings.HasPrefix(base, "wss://"):
		scheme, rest = "wss://", base[len("wss://"):]
	case strings.HasPrefix(base, "ws://"):
		scheme, rest = "ws://", base[len("ws://"):]
	}
	host := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host = rest[:i] // drop any path; we force /ws
	}
	// Use the raw id (query-escaped) like v1 — NOT uuid.MustParse, which would
	// panic the whole process if HOME_SITE_ID env were a non-UUID. A bad id at
	// worst fails to match a cloud binding; it must never crash the adapter.
	q := "role=home_adapter&home_site_id=" + url.QueryEscape(c.HomeSiteID)
	if c.CloudAuthToken != "" {
		q += "&token=" + url.QueryEscape(c.CloudAuthToken)
	}
	return fmt.Sprintf("%s%s/ws?%s", scheme, host, q)
}

// Relay is the running cloud-relay client. It owns the device→PTY bridges.
type Relay struct {
	cfg     Config
	dev     *butler.DeviceSession // active-conversation lifecycle (ADR-032)
	bridges *bridgeManager        // PTY session + Bridge onto session.DefaultID
	log     func(format string, args ...any)
}

// New builds a Relay sharing the given session.Manager (so the device's voice
// turns drive the SAME default session the web terminal joins). dev owns the
// default conversation's spawn config (persona + permissions + the active
// conversation's resume flag) and the new/list/resume operations. log is the line
// logger (e.g. log.Printf).
func New(mgr *session.Manager, dev *butler.DeviceSession, cfg Config, log func(string, ...any)) *Relay {
	return &Relay{cfg: cfg, dev: dev, bridges: newBridgeManager(mgr, dev), log: log}
}

// Run connects to the cloud and serves relayed requests until ctx is cancelled,
// reconnecting with a fixed backoff on any drop. It blocks; run it in a goroutine.
func (r *Relay) Run(ctx context.Context) {
	r.log("cloudrelay: home_site=%s dialing %s", r.cfg.HomeSiteID, r.cfg.CloudWSURL)
	for ctx.Err() == nil {
		if err := r.runOnce(ctx); err != nil && ctx.Err() == nil {
			r.log("cloudrelay: connection ended: %v (reconnecting in %s)", err, r.cfg.ReconnectDelay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.ReconnectDelay):
		}
	}
}

// runOnce dials once and serves frames until the connection drops or ctx ends.
func (r *Relay) runOnce(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, r.cfg.dialURL(), nil)
	if err != nil {
		return fmt.Errorf("dial cloud: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Per-connection context: cancelled when this connection ends, so any in-flight
	// turn handler unblocks (releasing its per-device turn lock) instead of holding
	// it until ReplyWait after the cloud drops.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// All writes go through one mutex (a WS conn is not safe for concurrent writes).
	var writeMu sync.Mutex
	write := func(env Envelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return wsjson.Write(wctx, conn, env)
	}

	// App-level JSON ping keepalive.
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go func() {
		t := time.NewTicker(appPingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				if write(Envelope{Type: "ping"}) != nil {
					return
				}
			}
		}
	}()

	r.log("cloudrelay: connected home_site=%s", r.cfg.HomeSiteID)
	for {
		var env Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return fmt.Errorf("read cloud frame: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(env.Type)) {
		case "welcome":
			r.onWelcome(env, write)
		case "ack":
			// nothing to do
		case "event":
			if strings.EqualFold(strings.TrimSpace(env.Kind), "registration.code") {
				r.announceCode(env)
			}
		case "request":
			go r.handleRequest(connCtx, write, env)
		}
	}
}

// onWelcome logs the registration state and reports adapter info (display-only on
// the cloud, but v1 always sends it after welcome).
func (r *Relay) onWelcome(env Envelope, write func(Envelope) error) {
	if reg, _ := env.Payload["homeAdapterRegistration"].(string); strings.TrimSpace(reg) != "" {
		r.log("cloudrelay: welcome home_site=%s registration=%s", r.cfg.HomeSiteID, reg)
	}
	_ = write(Envelope{
		Type:       "info",
		HomeSiteID: r.cfg.HomeSiteID,
		Payload: map[string]any{
			"adapterVersion": buildinfo.Tag,
			"buildTime":      buildinfo.BuildTime,
			"platform":       "adapter_v2",
		},
	})
}

// announceCode prints the claim code the user redeems in the portal to bind this
// adapter to their account (so it appears in their device's sites.list).
func (r *Relay) announceCode(env Envelope) {
	code, _ := env.Payload["code"].(string)
	if strings.TrimSpace(code) == "" {
		return
	}
	expires, _ := env.Payload["expiresAt"].(string)
	r.log("cloudrelay: ┌──────────────────────────────────────────────┐")
	r.log("cloudrelay: │ PAIRING REQUIRED — claim this adapter_v2     │")
	r.log("cloudrelay: │   code: %-36s │", code)
	r.log("cloudrelay: │   expires: %-33s │", expires)
	r.log("cloudrelay: │ Redeem in the BBClaw portal:                 │")
	r.log("cloudrelay: │   POST /v1/registrations/claim {\"code\":...}  │")
	r.log("cloudrelay: └──────────────────────────────────────────────┘")
}

// handleRequest dispatches a cloud request. voice.transcript is the voice path;
// the agent.* / chat.* / menu kinds are the device settings-page proxy requests,
// answered with minimal static replies so the settings UI doesn't hang. Anything
// else is silently ignored (the cloud tolerates a no-op, like v1's default).
func (r *Relay) handleRequest(ctx context.Context, write func(Envelope) error, env Envelope) {
	// Record the cloud device id so the `device set-volume/set-miyu` CLI can target
	// "the current device" without the butler knowing its id (curdevice). This is
	// the device id the cloud config API expects. Unchanged ids are a no-op write.
	_ = curdevice.Record(env.DeviceID)
	if strings.EqualFold(strings.TrimSpace(env.Kind), "voice.transcript") {
		if err := r.handleTranscript(ctx, write, env); err != nil {
			r.log("cloudrelay: transcript device=%s error: %v", env.DeviceID, err)
			_ = write(Envelope{
				Type: "reply", MessageID: env.MessageID, DeviceID: env.DeviceID,
				HomeSiteID: r.cfg.HomeSiteID, Kind: "voice.reply",
				Payload: map[string]any{"ok": false, "error": err.Error()},
			})
		}
		return
	}
	// turn.cancel is the device's barge-in/abort signal (PTT pressed during a
	// reply). Interrupt the in-flight turn so a stuck/slow turn (e.g. a long tool
	// call, or claude stuck on an API retry) doesn't run until ReplyWait — before
	// this the cancel was ignored ("no handler") and the device sat in "waiting"
	// until the turn timed out.
	if strings.EqualFold(strings.TrimSpace(env.Kind), "turn.cancel") {
		r.handleTurnCancel(write, env)
		return
	}
	// prompt.select is the device's answer to a forwarded blocking menu (ADR-033) —
	// an INDEPENDENT request kind (not voice.transcript), so a human's think-time is
	// decoupled from ReplyWait and from supersede-on-new-transcript. Route it to the
	// live bridge; a stale/unknown promptId is a safe no-op there.
	if strings.EqualFold(strings.TrimSpace(env.Kind), "prompt.select") {
		r.handlePromptSelect(write, env)
		return
	}
	// Settings/UI proxy kinds (agent.drivers, agent.messages, agent.menu, …) get a
	// minimal static reply; unknown kinds fall through to a silent no-op. Log either
	// way so the device's settings-page traffic is visible while debugging.
	if r.handleAgentProxy(write, env) {
		r.log("cloudrelay: proxy device=%s kind=%s", env.DeviceID, env.Kind)
	} else {
		r.log("cloudrelay: ignored device=%s kind=%s (no handler)", env.DeviceID, env.Kind)
	}
}

// ── small helpers ───────────────────────────────────────────────────────────

func getOrDefault(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

type identityFile struct {
	HomeSiteID string `json:"homeSiteId"`
}

// ensureHomeSiteID returns HOME_SITE_ID if set, else loads/creates a persisted
// UUID at ~/.bbclaw-adapter-v2/identity.json so the claimed binding survives
// restarts. Uses a v2-specific dir so it does not collide with v1's site id
// (adapter_v2 is a SEPARATE site in the picker).
func ensureHomeSiteID() (string, error) {
	if id := strings.TrimSpace(os.Getenv("HOME_SITE_ID")); id != "" {
		return id, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	path := filepath.Join(home, ".bbclaw-adapter-v2", "identity.json")
	if raw, err := os.ReadFile(path); err == nil {
		var f identityFile
		if json.Unmarshal(raw, &f) == nil && strings.TrimSpace(f.HomeSiteID) != "" {
			return strings.TrimSpace(f.HomeSiteID), nil
		}
	}
	id := uuid.New().String()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create identity dir: %w", err)
	}
	data, _ := json.Marshal(identityFile{HomeSiteID: id})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write identity: %w", err)
	}
	return id, nil
}
