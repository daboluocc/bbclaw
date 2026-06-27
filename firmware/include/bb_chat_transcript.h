#pragma once

#include "lvgl.h"

/**
 * CHAT transcript: scrollable message bubble container.
 *
 * Supports 4 message types:
 *  - user        (right-aligned, accent green)
 *  - assistant   (left-aligned, accent blue, streaming append)
 *  - tool call   (left-aligned, dim, italic)
 *  - error       (left-aligned, red)
 *
 * Also supports history replay (append at tail, prepend at head for
 * scroll-to-top lazy loading).
 *
 * Single instance — owned by the active CHAT view in bb_lvgl_display.c.
 */

/* Construct transcript container as a child of `parent`.
 * Dimensions: width = full `parent` width, height = `height_px`. */
lv_obj_t* bb_chat_transcript_create(lv_obj_t* parent, int width, int height_px,
                                    int y_offset);

/* Destroy transcript container and reset internal state. */
void bb_chat_transcript_destroy(void);

/* Get the raw LVGL container (for scrollbar mode, flex config tweaks). */
lv_obj_t* bb_chat_transcript_get_container(void);

/* Message appenders. Streaming assistant chunks coalesce into a single bubble. */
void bb_chat_transcript_append_user(const char* text);

/* ADR-040 — re-add the user bubble for an adapter-committed turn (turn.committed)
 * ONLY if the live user turn was dropped (e.g. a barge-in withdrew it). No-op
 * when a live user bubble is already present, so the optimistic asr.final bubble
 * is never duplicated. Reconciles the device against the adapter's ground truth. */
void bb_chat_transcript_reconcile_user(const char* text);
void bb_chat_transcript_append_assistant_chunk(const char* delta);
void bb_chat_transcript_append_tool_call(const char* tool, const char* hint);
void bb_chat_transcript_append_error(const char* msg);

/* History replay — does not touch the active_assistant streaming bubble.
 * timestamp_ms: Unix timestamp in milliseconds (0 = unknown). When non-zero,
 * a time-segment separator is automatically inserted before the message if
 * the gap since the previous message exceeds BB_HISTORY_SEGMENT_GAP_MS. */
void bb_chat_transcript_append_history(const char* role, const char* content,
                                       int64_t timestamp_ms);
void bb_chat_transcript_prepend_history(const char* role, const char* content,
                                        int64_t timestamp_ms);

/* Scroll helpers. `lines > 0` = scroll down; `< 0` = up.
 * ADR-017: UP latches the transcript into reading mode (auto-scroll on new
 * messages is suspended). Scrolling all the way back to the bottom releases
 * the latch. `is_following` reports the current latch state. */
void bb_chat_transcript_scroll(int lines);
void bb_chat_transcript_scroll_to_bottom(void);
int  bb_chat_transcript_is_at_top(void);
int  bb_chat_transcript_is_following(void);
void bb_chat_transcript_resume_follow(void);

/* Finalize the streaming assistant bubble (so next append_assistant_chunk
 * starts a new bubble). Called after turn_end. */
void bb_chat_transcript_finalize_assistant(void);

/* ADR-017 — wipe all rendered messages without destroying the container.
 * Used by the cache-reconcile path when authoritative remote history
 * arrives and the cached preview needs to be replaced wholesale. */
void bb_chat_transcript_clear(void);

/* ADR-028 §2.5.1 (撤回语义) — withdraw the last live turn: delete the most
 * recent user bubble (set by append_user, not history replay) and everything
 * after it (the cancelled turn's in-flight reply/tool/error), and drop the
 * same turn from the persistence cache. Keeps earlier completed turns. No-op
 * when there is no live turn. Must run on the LVGL task (mutates objects). */
void bb_chat_transcript_withdraw_last_turn(void);

/* ADR-017 v2 — nav fast-path. Stream task can't poll UP/DOWN during TTS
 * playback (i2s write keeps it busy), so on_nav_event posts scroll
 * requests directly into this queue. A dedicated worker reads the queue
 * and bounces the call through lv_async_call, which the LVGL task
 * services between chunk-dispatch iterations even under TTS load.
 * `lines` follows bb_chat_transcript_scroll: <0 = up, >0 = down. */
void bb_chat_scroll_worker_init(void);
void bb_chat_scroll_request(int lines);

/* Issue #169 — TTS subtitle bar.
 *
 * A fixed-height label overlaid on the transcript area that shows the
 * current TTS segment being spoken.  Independent of the scrollable bubble
 * history — the history stays intact while the subtitle reflects the live
 * playback position.
 *
 * set_subtitle: replace the subtitle text with `text` and make it visible.
 *               `text` is copied; caller may free/reuse after the call.
 * clear_subtitle: hide the subtitle bar (called when TTS finishes or is
 *               cancelled, after the N-second hold timeout).
 *
 * Both functions must be called from the LVGL task (via lv_async_call from
 * worker tasks). */
void bb_chat_transcript_set_subtitle(const char* text);
void bb_chat_transcript_clear_subtitle(void);
