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

func (w *wrapper) rewrite(event *openclaw.VoiceTranscriptEvent) {
	text := strings.TrimSpace(event.Text)
	if vcmd := voicecmd.Match(text); vcmd != nil {
		w.log.Infof("pipeline: voice_command transcript=%q cmd=%s stream=%s",
			text, vcmd.Command, event.StreamID)
		w.metrics.Inc("voice_command_intercepted")
		event.Text = vcmd.Command
	}
}

func (w *wrapper) SendVoiceTranscript(ctx context.Context, event openclaw.VoiceTranscriptEvent) (openclaw.VoiceTranscriptDelivery, error) {
	w.rewrite(&event)
	return w.inner.SendVoiceTranscript(ctx, event)
}

func (w *wrapper) SendVoiceTranscriptStream(
	ctx context.Context,
	event openclaw.VoiceTranscriptEvent,
	onEvent func(openclaw.VoiceTranscriptStreamEvent),
) (openclaw.VoiceTranscriptDelivery, error) {
	w.rewrite(&event)
	return w.inner.SendVoiceTranscriptStream(ctx, event, onEvent)
}
