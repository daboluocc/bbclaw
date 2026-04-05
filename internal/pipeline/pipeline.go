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

// agentSender is optionally implemented by sinks that can send slash
// commands via the Gateway "agent" RPC method (chat path).
type agentSender interface {
	SendAgentMessage(ctx context.Context, message, sessionKey string) error
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
	if cmd := w.matchCommand(ctx, event); cmd != "" {
		return openclaw.VoiceTranscriptDelivery{ReplyText: cmd}, nil
	}
	return w.inner.SendVoiceTranscript(ctx, event)
}

func (w *wrapper) SendVoiceTranscriptStream(
	ctx context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	if cmd := w.matchCommand(ctx, event); cmd != "" {
		if onEvent != nil {
			onEvent(openclaw.VoiceTranscriptStreamEvent{Type: "reply.delta", Text: cmd})
		}
		return openclaw.VoiceTranscriptDelivery{ReplyText: cmd}, nil
	}
	return w.inner.SendVoiceTranscriptStream(ctx, event, onEvent)
}

// matchCommand checks if the transcript is a voice command. If so, it sends
// the slash command via the "agent" method (chat path) and returns the command
// string. Returns "" if not a voice command.
func (w *wrapper) matchCommand(ctx context.Context, event openclaw.VoiceTranscriptEvent) string {
	text := strings.TrimSpace(event.Text)
	vcmd := voicecmd.Match(text)
	if vcmd == nil {
		return ""
	}
	w.log.Infof("pipeline: voice_command transcript=%q cmd=%s stream=%s",
		text, vcmd.Command, event.StreamID)
	w.metrics.Inc("voice_command_intercepted")

	sender, ok := w.inner.(agentSender)
	if !ok {
		w.log.Warnf("pipeline: inner sink does not support SendAgentMessage, falling back to voice.transcript")
		event.Text = vcmd.Command
		return ""
	}
	if err := sender.SendAgentMessage(ctx, vcmd.Command, event.SessionKey); err != nil {
		w.log.Errorf("pipeline: SendAgentMessage failed cmd=%s err=%v", vcmd.Command, err)
		return ""
	}
	return vcmd.Command
}
