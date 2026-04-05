// Package pipeline wraps the OpenClaw transcript sink with shared
// pre-processing logic (voice commands, future: text filters, transforms).
// Both bbclaw-adapter and bbclaw-home-adapter use this as their sink.
package pipeline

import (
	"context"
	"strings"

	"github.com/zhoushoujianwork/bbclaw/adapter/internal/obs"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/openclaw"
	"github.com/zhoushoujianwork/bbclaw/adapter/internal/voicecmd"
)

// Sink is the interface both adapter entry points already depend on.
type Sink interface {
	SendVoiceTranscript(ctx context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error)
	SendVoiceTranscriptStream(
		ctx context.Context,
		event openclaw.VoiceTranscriptEvent,
		onEvent func(openclaw.VoiceTranscriptStreamEvent),
	) (openclaw.VoiceTranscriptDelivery, error)
}

// commandSender can send slash commands via the OpenAI-compatible HTTP API.
type commandSender interface {
	SendSlashCommand(ctx context.Context, command, sessionKey string) (string, error)
}

// Wrap returns a Sink that applies shared pre-processing before delegating
// to the underlying OpenClaw sink.
func Wrap(inner Sink, log *obs.Logger, metrics *obs.Metrics) Sink {
	return &wrapper{inner: inner, log: log, metrics: metrics}
}

type wrapper struct {
	inner   Sink
	log     *obs.Logger
	metrics *obs.Metrics
}

func (w *wrapper) SendVoiceTranscript(ctx context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error) {
	if reply := w.handleCommand(ctx, event); reply != "" {
		return openclaw.VoiceTranscriptDelivery{ReplyText: reply}, nil
	}
	return w.inner.SendVoiceTranscript(ctx, event)
}

func (w *wrapper) SendVoiceTranscriptStream(
	ctx context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	if reply := w.handleCommand(ctx, event); reply != "" {
		if onEvent != nil {
			onEvent(openclaw.VoiceTranscriptStreamEvent{Type: "reply.delta", Text: reply})
		}
		return openclaw.VoiceTranscriptDelivery{ReplyText: reply}, nil
	}
	return w.inner.SendVoiceTranscriptStream(ctx, event, onEvent)
}

// handleCommand sends slash commands via the OpenAI-compatible HTTP API
// (POST /v1/chat/completions with Bearer token). This path sets
// senderIsOwner=true for shared-secret auth, so Gateway correctly parses
// slash commands like /status, /stop, /new.
func (w *wrapper) handleCommand(ctx context.Context, event openclaw.VoiceTranscriptEvent) string {
	text := strings.TrimSpace(event.Text)
	vcmd := voicecmd.Match(text)
	if vcmd == nil {
		return ""
	}
	w.log.Infof("pipeline: voice_command transcript=%q cmd=%s stream=%s",
		text, vcmd.Command, event.StreamID)
	w.metrics.Inc("voice_command_intercepted")

	sender, ok := w.inner.(commandSender)
	if !ok {
		w.log.Warnf("pipeline: inner sink does not support SendSlashCommand (type=%T)", w.inner)
		return ""
	}
	w.log.Infof("pipeline: sending slash command via HTTP cmd=%s session=%s", vcmd.Command, event.SessionKey)
	reply, err := sender.SendSlashCommand(ctx, vcmd.Command, event.SessionKey)
	if err != nil {
		w.log.Errorf("pipeline: SendSlashCommand failed cmd=%s err=%v", vcmd.Command, err)
		return ""
	}
	if reply == "" {
		reply = vcmd.Command
	}
	return reply
}
