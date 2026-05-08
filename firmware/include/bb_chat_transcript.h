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
 * Single instance — shared between buddy-anim overlay (Phase 4-6) and
 * the future bb_page_chat (Phase 7+).
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
void bb_chat_transcript_append_assistant_chunk(const char* delta);
void bb_chat_transcript_append_tool_call(const char* tool, const char* hint);
void bb_chat_transcript_append_error(const char* msg);

/* History replay — does not touch the active_assistant streaming bubble. */
void bb_chat_transcript_append_history(const char* role, const char* content);
void bb_chat_transcript_prepend_history(const char* role, const char* content);

/* Scroll helpers. `lines > 0` = scroll down; `< 0` = up. */
void bb_chat_transcript_scroll(int lines);
void bb_chat_transcript_scroll_to_bottom(void);
int  bb_chat_transcript_is_at_top(void);

/* Finalize the streaming assistant bubble (so next append_assistant_chunk
 * starts a new bubble). Called after turn_end. */
void bb_chat_transcript_finalize_assistant(void);
