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
