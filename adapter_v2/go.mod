module github.com/daboluocc/bbclaw/adapter_v2

go 1.22

// Phase 1 dependencies:
//   github.com/creack/pty   — PTY host (ptyhost)
//   github.com/hinshun/vt10x — server-side VT screen (vtscreen)
//   nhooyr.io/websocket     — raw byte terminal channel (termchan)
// Reused-from-v1 packages (asr/tts/audio/config) are pulled in when the
// device channel (Phase 2) lands — either copied or extracted into a shared lib.

require (
	github.com/creack/pty v1.1.24
	github.com/daboluocc/bbclaw/voice v0.0.0
	github.com/hinshun/vt10x v0.0.0-20220301184237-5011da428d02
	nhooyr.io/websocket v1.8.17
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
)

replace github.com/daboluocc/bbclaw/voice => ../voice
