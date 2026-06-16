# ADR-030 — Device-side execution steps (display-only step channel)

**Status**: Accepted · 2026-06-17
**Scope**: firmware (conversation page) · adapter (voice/butler relay) · cloud (relay passthrough)

## Context

When the butler runs a turn that does real work — e.g. the user says
「把声音设置到20」 and the agent calls `Bash: bbclaw-adapter device set-volume 20`
— the device today either says nothing about the step, or (on the legacy radio
transcript) speaks/show only the bare tool name. Two problems:

1. **No visible steps on the conversation page.** The conversation page handler
   (`on_finish_stream_event_tts_only`) renders `reply.delta` / ASR / TTS but
   ignores `tool_call` and `thinking` frames, so the screen shows nothing while
   the agent is acting.
2. **TTS would speak the wrong thing if we naively widened it.** We do **not**
   want the device to read aloud every in-progress line ("正在调用…", tool
   names, status). The user wants to *hear* only the main result
   (「音量已调到 20%」) but *see* the brief steps that produced it.

The wire protocol already has the right shape — it just isn't fully used:

- **Speak channel**: `reply.delta` (streamed) + final `voice.reply` `{text}`.
  Cloud synthesizes TTS from *this text only*.
- **Step channel** (display-only): `tool_call`, `thinking`, `dispatch_status`.
  These never enter `voice.reply`, so they are never spoken.

So the frame `type` itself is the "speak vs display" marker the feature needs.
What was missing: the step frames carried no useful text (tool *name* only, no
hint), and the conversation page didn't render them.

## Decision

Formalize a **two-channel contract** and render the step channel on the
conversation page.

### Channel contract

| Channel | Frames | Spoken (TTS) | Displayed |
|---|---|---|---|
| **Speak** (main content) | `reply.delta`, `voice.reply` | ✅ yes | ✅ yes |
| **Step** (display-only) | `tool_call`, `thinking`, `dispatch_status` | ❌ never | ✅ yes |

The backend never needs to decide "should I speak this?" per-token: it routes
model prose to the speak channel and progress to the step channel. TTS is
driven solely by the speak channel, unchanged. The device renders both.

### Step frame format (enriched)

`tool_call` frames now carry a short, human-readable **hint** in addition to the
tool name:

```json
{ "type": "tool_call", "name": "Bash", "hint": "bbclaw-adapter device set-volume 20" }
```

- `name` — tool name (`Bash` / `Edit` / `Read` / …).
- `hint` — short preview (the command, file path, …), already produced by the
  Claude Code driver's `summarizeToolInput` (capped 80 chars). May be empty.

Cloud→adapter envelope kind stays `tool_call`; payload gains the `hint` field.
This is **backward compatible**: older firmware reads `name` and ignores `hint`;
older adapters that don't send `hint` render as before.

`thinking` keeps `{ "text": ... }`. `dispatch_status` is unchanged (ADR-021).

### Rendering

The conversation page shows steps as a distinct, dimmed/italic line, e.g.

```
[tool] Bash: bbclaw-adapter device set-volume 20
```

via the existing `bb_chat_transcript_append_tool_call(tool, hint)`. Steps are
visually subordinate to the assistant's spoken reply. `thinking` is **not**
shown on the conversation page (kept minimal per the "简要" requirement); only
tool steps are surfaced there. The legacy radio transcript continues to inline
both, now including the hint.

### What is NOT changed

- The cloud TTS pipeline. It still synthesizes only `voice.reply` text.
- The butler system prompt. `DeviceSystemPrompt` already keeps spoken replies
  short/speakable (ADR-018 §3); the model is expected to put the result in prose
  and let tool steps speak for the "how".

## Cross-component touch points

| Layer | File | Change |
|---|---|---|
| Adapter | `internal/homeadapter/adapter.go` | `voiceEventSink.EmitEvent` + `handleChatTextViaAgent`: emit `tool_call` with `{name, hint}` (was name only) |
| Cloud | `cloud/internal/httpapi/server.go` | voice relay `case "tool_call"`: pass through `hint` |
| Firmware | `include/bb_adapter_client.h` | add `hint` to `bb_finish_stream_event_t` |
| Firmware | `src/bb_adapter_client.c` | parse `hint` in NDJSON + WS `tool_call` paths; carry through emit helper |
| Firmware | `src/bb_radio_app.c` | conversation handler renders TOOL_CALL as a step; radio handler includes hint |
| Firmware | `include/bb_ui_agent_chat.h` / `src/bb_ui_agent_chat.c` | public `bb_ui_agent_chat_post_tool_call(tool, hint)` |

## Consequences

- The conversation page now mirrors what the user saw in the 管家 web UI: a
  brief step line per tool call, with the spoken/displayed result as prose.
- TTS noise is eliminated by construction — progress never reaches the speak
  channel, so the device never reads tool names or "正在执行…" aloud.
- Adding future step kinds (e.g. richer status) is a matter of routing them to
  the step channel; no TTS logic changes.
