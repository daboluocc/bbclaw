package devicehub

import (
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
)

func TestHubSetActiveClear(t *testing.T) {
	h := New()
	if h.Active() != nil {
		t.Fatal("new hub should have no active bridge")
	}
	// Two distinct (half-wired) bridges stand in for two device lines.
	b1 := deviceapi.New(nil, nil, nil, nil, deviceapi.Config{})
	b2 := deviceapi.New(nil, nil, nil, nil, deviceapi.Config{})

	h.Set(b1)
	if h.Active() != b1 {
		t.Fatal("Set(b1) did not become active")
	}
	// Last online wins.
	h.Set(b2)
	if h.Active() != b2 {
		t.Fatal("Set(b2) did not replace b1")
	}
	// A stale teardown of b1 must NOT wipe b2 (compare-and-clear).
	h.Clear(b1)
	if h.Active() != b2 {
		t.Fatal("stale Clear(b1) wiped the active b2")
	}
	// Clearing the actual active bridge empties the slot.
	h.Clear(b2)
	if h.Active() != nil {
		t.Fatal("Clear(b2) should have emptied the slot")
	}
}
