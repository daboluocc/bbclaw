package agent

import (
	"context"
	"testing"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// stubDriver is a minimal Driver used only by router unit tests. It does no
// real work; every call returns a default value.
type stubDriver struct {
	name string
	caps Capabilities
}

func (s *stubDriver) Name() string                                           { return s.name }
func (s *stubDriver) Capabilities() Capabilities                             { return s.caps }
func (s *stubDriver) Start(context.Context, StartOpts) (SessionID, error)    { return SessionID(s.name + "-sid"), nil }
func (s *stubDriver) Send(SessionID, string) error                           { return nil }
func (s *stubDriver) Events(SessionID) <-chan Event                          { ch := make(chan Event); close(ch); return ch }
func (s *stubDriver) Approve(SessionID, ToolID, Decision) error              { return ErrUnsupported }
func (s *stubDriver) Stop(SessionID) error                                   { return nil }

func TestRouter_RegisterAndLookup(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()

	d1 := &stubDriver{name: "alpha", caps: Capabilities{Streaming: true, MaxInputBytes: 1024}}
	d2 := &stubDriver{name: "beta", caps: Capabilities{Resume: true}}

	r.Register(d1, log)
	r.Register(d2, log)

	if got, ok := r.Get("alpha"); !ok || got != d1 {
		t.Fatalf("Get alpha: got=%v ok=%v", got, ok)
	}
	if got, ok := r.Get("beta"); !ok || got != d2 {
		t.Fatalf("Get beta: got=%v ok=%v", got, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get missing: should be false")
	}

	// First registered wins as default.
	if def := r.Default(); def != d1 {
		t.Fatalf("Default: got=%v want=%v", def, d1)
	}

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List len=%d want=2", len(list))
	}
	seen := map[string]Capabilities{}
	for _, info := range list {
		seen[info.Name] = info.Capabilities
	}
	if seen["alpha"].MaxInputBytes != 1024 || !seen["alpha"].Streaming {
		t.Errorf("alpha caps roundtrip wrong: %+v", seen["alpha"])
	}
	if !seen["beta"].Resume {
		t.Errorf("beta caps roundtrip wrong: %+v", seen["beta"])
	}
}

func TestRouter_DuplicateRegisterOverwrites(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()

	d1 := &stubDriver{name: "x"}
	d2 := &stubDriver{name: "x"} // same name

	r.Register(d1, log)
	r.Register(d2, log) // should log a warning and overwrite

	if got, _ := r.Get("x"); got != d2 {
		t.Fatalf("duplicate register: got=%v want=%v (overwrite)", got, d2)
	}
	// Default was pinned on first Register; a same-name overwrite keeps the
	// name as default but now points at d2.
	if def := r.Default(); def != d2 {
		t.Fatalf("Default after overwrite: got=%v want=%v", def, d2)
	}
}

func TestRouter_EmptyDefaultIsNil(t *testing.T) {
	r := NewRouter()
	if def := r.Default(); def != nil {
		t.Fatalf("empty router Default: got=%v want=nil", def)
	}
	if list := r.List(); len(list) != 0 {
		t.Fatalf("empty router List: got=%v want empty", list)
	}
}

// Nil Register is a no-op (guards against wiring mistakes at startup).
func TestRouter_NilRegisterNoop(t *testing.T) {
	r := NewRouter()
	r.Register(nil, obs.NewLogger())
	if r.Default() != nil {
		t.Fatal("nil Register should not set default")
	}
	if len(r.List()) != 0 {
		t.Fatal("nil Register should not add entries")
	}
}

// stubResolver is a minimal SessionResolver for testing SendSlashCommand.
type stubResolver struct {
	driverName string
	sid        SessionID
	found      bool
	resetCalls []string
	stopCalled bool
}

func (s *stubResolver) ResolveSession(sessionKey string) (string, SessionID, bool) {
	return s.driverName, s.sid, s.found
}

func (s *stubResolver) ResetSession(sessionKey string) {
	s.resetCalls = append(s.resetCalls, sessionKey)
}

func TestRouter_SendSlashCommand_Stop(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()
	d := &stubDriver{name: "alpha"}
	r.Register(d, log)

	// /stop with a live session should call Stop on the driver.
	resolver := &stubResolver{driverName: "alpha", sid: "alpha-sid", found: true}
	r.SetSessionResolver(resolver)

	ctx := context.Background()
	reply, err := r.SendSlashCommand(ctx, "/stop", "ls-abc")
	if err != nil {
		t.Fatalf("/stop unexpected error: %v", err)
	}
	// reply is empty — pipeline layer adds the confirmation text.
	if reply != "" {
		t.Errorf("/stop reply want empty got %q", reply)
	}
}

func TestRouter_SendSlashCommand_StopNoSession(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()
	r.Register(&stubDriver{name: "alpha"}, log)

	// /stop with no live session is a no-op, not an error.
	resolver := &stubResolver{found: false}
	r.SetSessionResolver(resolver)

	ctx := context.Background()
	reply, err := r.SendSlashCommand(ctx, "/stop", "ls-missing")
	if err != nil {
		t.Fatalf("/stop no-session unexpected error: %v", err)
	}
	if reply != "" {
		t.Errorf("/stop no-session reply want empty got %q", reply)
	}
}

func TestRouter_SendSlashCommand_New(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()
	r.Register(&stubDriver{name: "alpha"}, log)

	resolver := &stubResolver{found: false}
	r.SetSessionResolver(resolver)

	ctx := context.Background()
	reply, err := r.SendSlashCommand(ctx, "/new", "ls-abc")
	if err != nil {
		t.Fatalf("/new unexpected error: %v", err)
	}
	if reply != "" {
		t.Errorf("/new reply want empty got %q", reply)
	}
	if len(resolver.resetCalls) != 1 || resolver.resetCalls[0] != "ls-abc" {
		t.Errorf("/new: ResetSession not called with correct key, got %v", resolver.resetCalls)
	}
}

func TestRouter_SendSlashCommand_Status(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()
	r.Register(&stubDriver{name: "alpha"}, log)

	// With a live session, /status includes driver + session id.
	resolver := &stubResolver{driverName: "alpha", sid: "alpha-sid", found: true}
	r.SetSessionResolver(resolver)

	ctx := context.Background()
	reply, err := r.SendSlashCommand(ctx, "/status", "ls-abc")
	if err != nil {
		t.Fatalf("/status unexpected error: %v", err)
	}
	if !contains(reply, "alpha") {
		t.Errorf("/status reply %q should contain driver name", reply)
	}
}

func TestRouter_SendSlashCommand_StatusNoResolver(t *testing.T) {
	r := NewRouter()
	log := obs.NewLogger()
	r.Register(&stubDriver{name: "beta"}, log)
	// No resolver set — /status falls back to default driver name.

	ctx := context.Background()
	reply, err := r.SendSlashCommand(ctx, "/status", "")
	if err != nil {
		t.Fatalf("/status no-resolver unexpected error: %v", err)
	}
	if !contains(reply, "beta") {
		t.Errorf("/status no-resolver reply %q should contain driver name", reply)
	}
}

func TestRouter_SendSlashCommand_Unknown(t *testing.T) {
	r := NewRouter()
	r.Register(&stubDriver{name: "alpha"}, obs.NewLogger())

	ctx := context.Background()
	_, err := r.SendSlashCommand(ctx, "/unknown", "")
	if err == nil {
		t.Fatal("/unknown should return error")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
