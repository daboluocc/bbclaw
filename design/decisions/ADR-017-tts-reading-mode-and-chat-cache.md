# ADR-017 — TTS Reading Mode & Chat Tail Cache

**Date**: 2026-05-18
**Status**: Implemented
**Related**: ADR-013 (Session History Replay), ADR-014 (Logical Session), ADR-016 (Driver/Model Selection)

## Context

Two UX problems on the chat overlay had the same root: the transcript only
exists in LVGL widgets, and live behaviour kept stomping the user's intent.

1. **TTS playback hijacked the transcript scroll.** Every streaming
   assistant chunk called `scroll_to_bottom()` in `bb_chat_transcript.c`,
   so any UP gesture the user made during TTS was immediately yanked back
   within the next chunk (~100 ms cadence). Users couldn't review earlier
   messages while a reply was being spoken.

2. **Sleep/wake wiped the visible history.** The transcript widget tree is
   destroyed in `theme_on_exit` (`lv_obj_del(s_st.root)` cascades to all
   bubble labels). Per ADR-013, history is the adapter's responsibility —
   the device re-fetches a page when `bb_ui_agent_chat_show` runs again.
   When the adapter is slow or unreachable at wake, the user sees a blank
   transcript and assumes their conversation is gone.

## Decision

### Part A — TTS reading mode (follow-tail latch)

`bb_chat_transcript` owns a single boolean `s_follow_tail`:

- New messages call `follow_tail_if_active()` instead of an unconditional
  `scroll_to_bottom()`. While the latch is set (default), behaviour is
  identical to before: every append scrolls into view.
- UP via `bb_chat_transcript_scroll(lines < 0)` releases the latch.
- DOWN that lands at the bottom (`lv_obj_get_scroll_bottom <= 4`)
  re-engages the latch.
- `bb_chat_transcript_scroll_to_bottom()` and a new
  `bb_chat_transcript_resume_follow()` force the latch back on.

On latch transitions, `bb_display_set_reading_hint(int)` toggles a bottom-
bar override that displays "● 阅读中 (DOWN 到底回到实时)" in the session
slot. The hint clears when the user rejoins the live tail.

**Why not repurpose OK as the resume button?** OK already opens the
session picker. DOWN-to-bottom is the natural gesture (it's literally
"scroll back to where new messages are arriving") and avoids overloading
an existing key.

### Part B — Chat tail cache (NVS blob, per-driver)

A new module `bb_chat_cache` keeps the last N messages of the active
session in an NVS blob keyed by driver (`cc/cc`, `cc/oc`, …). Layout:

```
uint32_t magic      = 0xBB17CACE
uint32_t version    = 1
uint16_t sid_len + char sid[sid_len]
uint16_t buf_len  + uint8_t buf[buf_len]      // packed records
```

Each record is `role:u8 | len:u16 | content[len]`. Roles are encoded as
single bytes (`u`/`a`/`t`/`e`). The blob is capped at 1.5 KB; the buffer
evicts whole messages from the head FIFO when a new append wouldn't fit.

**Why NVS rather than mounting the `resources` FAT partition?** FAT
requires mounting infrastructure (wear-levelling, `esp_vfs_fat_mount_rw_wl`,
LFN config) that nothing else uses today. NVS is already opened by every
other persistence path. The 24 KB partition has ~20 KB free; allocating
8 KB for chat caches (5 drivers × 1.5 KB) stays well within budget. The
1.5 KB per driver fits the **last few exchanges**, which is what the user
actually wants to see on wake.

**Why per-driver, single session?** The cache exists to bridge the gap
between wake-up and the adapter's first response — that gap only matters
for the session you're currently in. Earlier sessions still re-fetch on
demand via the existing pagination path (ADR-013).

### Storage hooks

- `bb_chat_cache_init()` called from `app_main`.
- `bb_chat_cache_bind(driver, sid)` called from
  - `load_nvs_task` (after `bb_session_store_load`) — captures the resumed
    session.
  - `apply_session_switch_ui` — picker / driver-cycle changes.
  - `BB_AGENT_EVENT_SESSION` handler — adapter-assigned new session.
- `bb_chat_cache_hydrate_from_nvs()` called from `load_nvs_task` (still
  on internal RAM stack — NVS reads disable SPI flash cache and panic on
  PSRAM stacks, see `bb_session_store.c` for the original incident).
- Transcript append paths (`bb_chat_transcript_append_user`,
  `bb_chat_transcript_append_assistant_chunk`, …) call into the cache.
  Streaming assistant chunks accumulate in `s_pending` and flush as one
  message via `finalize_assistant`.
- Persistence is deferred via `xTaskCreate(persist_task, ...)` — same
  pattern as `bb_session_store` so flash IO never runs under the LVGL
  lock.

### Reconcile with remote history

`on_history_fetch_done(is_initial=1)`:

1. The transcript may already be showing cached tail messages from the
   hydrate pass.
2. Call `bb_chat_transcript_clear()` to wipe the cached preview.
3. Call `bb_chat_cache_clear()` to drop the in-memory buffer.
4. Append remote messages via `theme->append_history_message` AND mirror
   each one into the cache.

Adapter remains the source of truth. If the fetch fails entirely (offline,
driver doesn't support replay), the user keeps the cached preview rather
than a blank transcript.

## Consequences

**Positive**
- Scroll works during TTS without changing the speech pipeline.
- Wake-up shows recent history immediately, even if the adapter is slow
  or briefly unreachable.
- No new partition / FS / dependency.

**Trade-offs**
- Cache is bounded to ~1.5 KB per driver. Long messages get truncated to
  480 B per record; the transcript already DOT-truncates anyway, so the
  visible result matches what was rendered.
- Switching to an older (non-active) session still requires the adapter
  to be reachable. The cache only covers the *current* session per
  driver.
- NVS writes happen on every finalized message. Wear is acceptable —
  bbclaw NVS partition is 24 KB with built-in wear levelling; even
  100 msg/day per driver is well within flash endurance.

**Future**
- If we want multi-session caching, mount the `resources` FAT partition
  and store one file per sid. Hooks are already factored so the cache
  module could grow a FAT-backed backend behind the same API.
- If the user wants an explicit "back to live" key in addition to
  DOWN-to-bottom, wire a button (e.g. PTT double-tap) to
  `bb_chat_transcript_resume_follow()`.
