package cloudrelay

import (
	"context"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/deviceapi"
	"github.com/daboluocc/bbclaw/adapter_v2/internal/session"
)

func findKind(envs []Envelope, kind string) *Envelope {
	for i := range envs {
		if envs[i].Kind == kind {
			return &envs[i]
		}
	}
	return nil
}

// TestCloudEventsPromptForwardAndPark: PromptOpen/PromptClosed forward the cloud
// frames and flip the parked flag (ADR-033 P2).
func TestCloudEventsPromptForwardAndPark(t *testing.T) {
	e := &cloudEvents{}
	var got []Envelope
	e.begin(func(env Envelope) error { got = append(got, env); return nil },
		Envelope{MessageID: "m1", DeviceID: "d1"}, "site")

	e.PromptOpen(deviceapi.PromptSpec{
		ID: "p1", Kind: "permission", Question: "Do you want to proceed?",
		Options:   []deviceapi.PromptOption{{Key: "1", Label: "Yes", Default: true}, {Key: "2", Label: "No"}},
		Mechanism: "digit",
	})
	if !e.isPromptPending() {
		t.Error("PromptOpen must set promptPending (parks the turn)")
	}
	open := findKind(got, "voice.prompt.open")
	if open == nil {
		t.Fatal("no voice.prompt.open event emitted")
	}
	if open.Payload["promptId"] != "p1" || open.Payload["question"] != "Do you want to proceed?" {
		t.Errorf("bad voice.prompt.open payload: %v", open.Payload)
	}
	if opts, _ := open.Payload["options"].([]map[string]any); len(opts) != 2 {
		t.Errorf("voice.prompt.open options = %v, want 2", open.Payload["options"])
	}

	e.PromptClosed("p1", "answered")
	if e.isPromptPending() {
		t.Error("PromptClosed must clear promptPending")
	}
	cl := findKind(got, "voice.prompt.close")
	if cl == nil || cl.Payload["reason"] != "answered" {
		t.Errorf("bad voice.prompt.close: %v", cl)
	}
}

// TestPreemptParkedByPrompt: a barge-in preempt no-ops while a menu is pending (so
// the tool behind it isn't ESC-aborted), and resumes once the menu closes.
func TestPreemptParkedByPrompt(t *testing.T) {
	e := &cloudEvents{}
	e.begin(func(Envelope) error { return nil }, Envelope{}, "")
	e.PromptOpen(deviceapi.PromptSpec{ID: "p1", Options: []deviceapi.PromptOption{{Key: "1"}, {Key: "2"}}})

	cb := &cloudBridge{ev: e}
	ctx, cancel := context.WithCancel(context.Background())
	cb.arm(cancel)

	cb.preempt() // parked → must NOT cancel
	select {
	case <-ctx.Done():
		t.Fatal("preempt cancelled the turn while a prompt was pending (must park)")
	default:
	}

	e.PromptClosed("p1", "answered")
	cb.preempt() // no longer parked → cancels
	select {
	case <-ctx.Done():
	default:
		t.Fatal("preempt should cancel once the prompt cleared")
	}
}

// preempt must stay nil-safe for a cloudBridge with no events (TestCloudBridgePreempt
// constructs one) — a regression guard for the ev==nil branch.
func TestPreemptNilEventsSafe(t *testing.T) {
	cb := &cloudBridge{}
	cb.preempt() // must not panic
}

// TestDriverCapsToolApprovalGate: toolApproval is advertised true only when
// forward-to-device is enabled (capability-negotiation gate §9).
func TestDriverCapsToolApprovalGate(t *testing.T) {
	t.Setenv("ADAPTER_V2_CONFIRM_ON_DEVICE", "0")
	if driverCaps()["toolApproval"] != false {
		t.Error("toolApproval should be false when ConfirmOnDevice off")
	}
	t.Setenv("ADAPTER_V2_CONFIRM_ON_DEVICE", "1")
	if driverCaps()["toolApproval"] != true {
		t.Error("toolApproval should be true when ConfirmOnDevice on")
	}
}

// TestHandlePromptSelectNoBridgeAcks: with no live bridge a prompt.select is a safe
// ack (never spawns a CLI).
func TestHandlePromptSelectNoBridgeAcks(t *testing.T) {
	r := &Relay{
		cfg:     Config{HomeSiteID: "site"},
		bridges: newBridgeManager(session.NewManager(), nil),
		log:     func(string, ...any) {},
	}
	var got Envelope
	r.handlePromptSelect(func(env Envelope) error { got = env; return nil },
		Envelope{MessageID: "m1", DeviceID: "d1", Kind: "prompt.select",
			Payload: map[string]any{"promptId": "p1", "optionKey": "1"}})
	if got.Kind != "prompt.select" || got.Payload["ok"] != true {
		t.Errorf("expected prompt.select ack ok=true, got %+v", got)
	}
}
