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
	if reply := w.handleCommand(event); reply != "" {
		return openclaw.VoiceTranscriptDelivery{ReplyText: reply}, nil
	}
	return w.inner.SendVoiceTranscript(ctx, event)
}

func (w *wrapper) SendVoiceTranscriptStream(
	ctx context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	if reply := w.handleCommand(event); reply != "" {
		if onEvent != nil {
			onEvent(openclaw.VoiceTranscriptStreamEvent{Type: "reply.delta", Text: reply})
		}
		return openclaw.VoiceTranscriptDelivery{ReplyText: reply}, nil
	}
	return w.inner.SendVoiceTranscriptStream(ctx, event, onEvent)
}

// handleCommand checks if the transcript is a voice command and handles it
// locally in the adapter. Returns the reply text, or "" if not a command.
//
// Why adapter-side: OpenClaw Gateway treats voice.transcript events as
// senderIsOwner=false, so slash commands in that path are silently ignored
// and sent to the LLM as plain text. Commands must be handled before
// reaching the Gateway.
func (w *wrapper) handleCommand(event openclaw.VoiceTranscriptEvent) string {
	text := strings.TrimSpace(event.Text)
	vcmd := voicecmd.Match(text)
	if vcmd == nil {
		return ""
	}
	w.log.Infof("pipeline: voice_command transcript=%q cmd=%s stream=%s",
		text, vcmd.Command, event.StreamID)
	w.metrics.Inc("voice_command_intercepted")

	switch vcmd.Command {
	case "/stop":
		return "已停止"
	case "/new":
		return "新对话已开始"
	case "/status":
		return "运行正常"
	default:
		return vcmd.Command
	}
}
