package main

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// adminURLFromAddr derives the local admin page URL from a listen address such as
// ":18080", "0.0.0.0:18080" or "127.0.0.1:18080". A wildcard/empty host is
// rewritten to 127.0.0.1 — the admin surface is loopback-only, so that is the
// only host a browser on this machine should hit.
func adminURLFromAddr(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		// addr may be just ":18080" without a parseable split, or a bare port.
		port = strings.TrimPrefix(strings.TrimSpace(addr), ":")
		host = ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/admin"
}

// openAdminEnabled reports whether the post-start browser open is enabled. It is
// on by default and disabled by BBCLAW_OPEN_ADMIN in {0,false,no,off}.
func openAdminEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BBCLAW_OPEN_ADMIN"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// maybeOpenAdminBrowser opens the local admin page in the default browser shortly
// after startup, once the listener has had a moment to bind. Best-effort and
// non-fatal: a headless host (no browser / no display) simply logs and moves on.
// Call in its own goroutine — it sleeps before launching.
func maybeOpenAdminBrowser(addr string, logger *obs.Logger) {
	if !openAdminEnabled() {
		return
	}
	url := adminURLFromAddr(addr)
	// Give ListenAndServe a beat to bind so the opened tab doesn't race the
	// listener and show a connection error.
	time.Sleep(700 * time.Millisecond)
	if err := openBrowser(url); err != nil {
		if logger != nil {
			logger.Infof("admin: auto-open skipped (%v); open %s manually", err, url)
		}
		return
	}
	if logger != nil {
		logger.Infof("admin: opened %s in your browser (disable with BBCLAW_OPEN_ADMIN=0)", url)
	}
}

// openBrowser launches the OS default handler for url. Returns the command error,
// if any. Supports macOS, Linux/BSD (xdg-open) and Windows.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, freebsd, …
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
