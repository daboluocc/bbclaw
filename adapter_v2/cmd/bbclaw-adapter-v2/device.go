package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/curdevice"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/settingsstore"
)

// runDeviceCmd implements `bbclaw-adapter-v2 device …` — a SHORT-LIVED process
// the butler spawns (via its Bash tool) to adjust the CURRENT device's settings
// through the cloud config API. It mirrors v1's `bbclaw-adapter device` (cloud
// path: POST /v1/devices/{id}/config → the cloud pushes config.update over WS →
// the firmware applies it). This is NOT the running server — main dispatches
// here BEFORE the HTTP server starts when argv[1] == "device".
//
// args is os.Args[2:] (everything after "device"). Returns a process exit code.
func runDeviceCmd(args []string) int {
	// Surface CLOUD_WS_URL / CLOUD_AUTH_TOKEN from settings.json into the env,
	// exactly as the server does on boot. A butler-spawned CLI already inherits the
	// server's exported env, but a hand-run CLI (fresh shell) needs this too.
	loadSettingsEnv()

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: bbclaw-adapter-v2 device <set-volume|set-miyu> …")
		return 2
	}
	switch sub := args[0]; sub {
	case "set-volume":
		return runSetVolume(args[1:])
	case "set-miyu":
		return runSetMiyu(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown device subcommand %q (want set-volume|set-miyu)\n", sub)
		return 2
	}
}

// loadSettingsEnv reads settings.json (if present) and exports its values into the
// process env so the cloud-config readers below see the configured credentials.
// Missing/corrupt file is fine: Open returns a usable store over the env defaults.
func loadSettingsEnv() {
	seed := settingsstore.FromEnv()
	p := filepath.Join(settingsstore.DataDir(), "settings.json")
	store, err := settingsstore.Open(p, seed)
	if err != nil {
		return // env (and shell-exported) values already apply
	}
	store.ExportEnv()
}

// setConfigRequest is the body for POST /v1/devices/{id}/config. Fields are
// pointers + omitempty so a command only sends the keys it intends to change —
// otherwise set-miyu would emit volumePct:0 and the cloud would mute the device
// (the same partial-update contract as v1).
type setConfigRequest struct {
	VolumePct   *int  `json:"volumePct,omitempty"`
	MiyuEnabled *bool `json:"miyuEnabled,omitempty"`
}

type setConfigResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Version     int  `json:"version"`
		VolumePct   int  `json:"volumePct"`
		MiyuEnabled bool `json:"miyuEnabled"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

func runSetVolume(args []string) int {
	fs := flag.NewFlagSet("set-volume", flag.ContinueOnError)
	var deviceID string
	fs.StringVar(&deviceID, "device", "", "target device id (default: current connected device)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: device set-volume <0-100> [--device <id>]")
		return 2
	}
	pct, err := strconv.Atoi(strings.TrimSpace(rest[0]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid volume %q: must be an integer 0-100\n", rest[0])
		return 2
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	id := resolveDeviceID(deviceID)
	if id == "" {
		fmt.Fprintln(os.Stderr, "no target device: none connected yet and --device not given")
		return 1
	}
	res, err := postDeviceConfig(id, setConfigRequest{VolumePct: &pct})
	if err != nil {
		return 1
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "volumePct": res.Data.VolumePct, "version": res.Data.Version})
	fmt.Println(string(out))
	return 0
}

func runSetMiyu(args []string) int {
	fs := flag.NewFlagSet("set-miyu", flag.ContinueOnError)
	var deviceID string
	fs.StringVar(&deviceID, "device", "", "target device id (default: current connected device)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: device set-miyu <on|off> [--device <id>]")
		return 2
	}
	enabled, err := parseOnOff(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid value %q: must be on or off\n", rest[0])
		return 2
	}

	id := resolveDeviceID(deviceID)
	if id == "" {
		fmt.Fprintln(os.Stderr, "no target device: none connected yet and --device not given")
		return 1
	}
	res, err := postDeviceConfig(id, setConfigRequest{MiyuEnabled: &enabled})
	if err != nil {
		return 1
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "miyuEnabled": res.Data.MiyuEnabled, "version": res.Data.Version})
	fmt.Println(string(out))
	return 0
}

// resolveDeviceID returns the explicit --device value when given, else the device
// the adapter most recently saw (curdevice). This is what lets the butler say
// "set my volume to 50" without knowing any id.
func resolveDeviceID(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	return curdevice.Get()
}

// parseOnOff parses a human on/off toggle argument into a bool (ported from v1).
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "enable", "enabled", "yes":
		return true, nil
	case "off", "false", "0", "disable", "disabled", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid on/off value: %s", s)
	}
}

// postDeviceConfig sends a partial config update to the cloud for deviceID and
// returns the decoded response. Shared by set-volume and set-miyu.
func postDeviceConfig(deviceID string, body setConfigRequest) (*setConfigResponse, error) {
	base, err := cloudHTTPBase()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot derive cloud HTTP base from CLOUD_WS_URL: %v\n", err)
		return nil, err
	}
	payload, _ := json.Marshal(body)
	u := base + "/v1/devices/" + url.PathEscape(deviceID) + "/config"
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "构建请求失败: %v\n", err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "请求云端失败: %v\n", err)
		return nil, err
	}
	defer resp.Body.Close()

	var result setConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "解析响应失败 (HTTP %d): %v\n", resp.StatusCode, err)
		return nil, err
	}
	if !result.OK || resp.StatusCode != http.StatusOK {
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		fmt.Fprintf(os.Stderr, "云端返回错误: %s\n", msg)
		return nil, fmt.Errorf("cloud error: %s", msg)
	}
	return &result, nil
}

// cloudHTTPBase derives the cloud's https base ("https://host") from CLOUD_WS_URL,
// falling back to the cloudrelay default. Mirrors v1's DeriveCloudHTTPBase.
func cloudHTTPBase() (string, error) {
	ws := strings.TrimSpace(os.Getenv("CLOUD_WS_URL"))
	if ws == "" {
		ws = "wss://bbclaw.daboluo.cc/ws" // same default as cloudrelay.LoadConfig
	}
	u, err := url.Parse(ws)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
		// already an http(s) base
	default:
		return "", fmt.Errorf("unsupported CLOUD_WS_URL scheme: %s", u.Scheme)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}
