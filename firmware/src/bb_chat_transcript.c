/**
 * CHAT transcript — message bubble rendering.
 *
 * Migrated from bb_theme_buddy_anim.c (make_msg_label + theme_append_* functions).
 * Keeps identical visual style: colored rounded bubbles, right-aligned user
 * messages, left-aligned assistant/tool/error messages.
 *
 * Streaming assistant chunks coalesce into a single bubble via lv_label_ins_text.
 */
#include "bb_chat_transcript.h"

#include <stdio.h>
#include <string.h>

#include "bb_chat_cache.h"
#include "bb_display.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

/* Color palette — design/UI_DESIGN_LANGUAGE.md tokens. Monochrome + teal:
 * user bubble = teal tint (right-aligned), assistant bubble = solid ghost
 * surface (left-aligned) — alignment + tone separate the speakers, no blue. */
#define UI_TEXT_MAIN   BB_UI_DOT_LIT
#define UI_TEXT_DIM    BB_UI_TEXT_DIM
#define UI_ME_ACCENT   BB_UI_ACCENT
#define UI_AI_SURFACE  BB_UI_DOT_GHOST /* assistant bubble face */

/* Time-segment threshold: two messages separated by more than this (30 min)
 * get a visual divider between them.  Must match adapter's GAP_MS constant
 * (adapter/web/src/components/Conversation.vue). */
#define BB_HISTORY_SEGMENT_GAP_MS  (30LL * 60 * 1000)
#define UI_TOOL_FG     BB_UI_TEXT_DIM
#define UI_ERROR_FG    BB_UI_ERR

#define MSG_PAD     4
#define MSG_RADIUS  6
#define MSG_HMARGIN 8

static lv_obj_t* s_transcript;
static lv_obj_t* s_active_assistant;  /* current streaming bubble, NULL after finalize */

/* Last-seen timestamp for history replay, used to detect segment gaps.
 * Tracks the most-recently appended message; reset to 0 on transcript clear. */
static int64_t s_history_last_ts_ms = 0;

/* ADR-017 — reading mode.
 *
 * `s_follow_tail` is the auto-scroll latch. When 1 (default), new messages
 * scroll the transcript to the bottom so the user sees the live stream.
 * When 0, the user manually scrolled away (UP) and we leave their viewport
 * alone — including during TTS playback, which previously yanked the view
 * back on every assistant chunk. Auto-resume kicks in when the user scrolls
 * all the way back to the bottom.
 *
 * The visible bottom-bar hint that v1 of this ADR added turned out to be
 * noisy in practice — users intuitively understand "I scrolled up, now I'm
 * looking at older messages" without a label. The latch still exists
 * internally (it's what makes scrolling actually work during TTS), but
 * the UI marker is gone. */
static int s_follow_tail = 1;

static int transcript_at_bottom(void) {
  if (s_transcript == NULL) return 1;
  return lv_obj_get_scroll_bottom(s_transcript) <= 4;
}

static void set_follow_tail(int follow) {
  follow = follow ? 1 : 0;
  if (s_follow_tail == follow) return;
  s_follow_tail = follow;
}

static const lv_font_t* font(void) {
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
}

static lv_obj_t* make_msg_label(uint32_t bg_color, uint32_t fg_color,
                                lv_text_align_t align, int italic) {
  if (s_transcript == NULL) return NULL;
  lv_obj_t* lbl = lv_label_create(s_transcript);
  /* DOT mode = single-line, ellipsize overflow. WRAP mode (which makes the
   * label's height a function of its width) combined with the parent's flex
   * column + scrollable triggers a layout oscillation in LVGL 9.5: any
   * forced layout pass (lv_snapshot_take, lv_obj_scroll_to_view,
   * lv_obj_update_layout) hangs the LVGL task. DOT keeps the label height
   * fixed at one line so flex resolves the column in a single pass.
   * Trade-off: long messages are truncated with "..." in the transcript;
   * full content is still in the session history backend. */
  lv_label_set_long_mode(lbl, LV_LABEL_LONG_MODE_DOTS);
  lv_obj_set_width(lbl, 320 - 2 * MSG_HMARGIN);
  lv_obj_set_style_text_font(lbl, font(), 0);
  lv_obj_set_style_text_color(lbl, lv_color_hex(fg_color), 0);
  lv_obj_set_style_text_align(lbl, align, 0);
  lv_obj_set_style_bg_color(lbl, lv_color_hex(bg_color), 0);
  lv_obj_set_style_bg_opa(lbl, LV_OPA_30, 0);
  lv_obj_set_style_radius(lbl, MSG_RADIUS, 0);
  lv_obj_set_style_pad_all(lbl, MSG_PAD, 0);
  lv_obj_set_style_margin_top(lbl, 2, 0);
  lv_obj_set_style_margin_bottom(lbl, 2, 0);
  if (italic) {
    lv_obj_set_style_text_opa(lbl, LV_OPA_70, 0);
  }
  /* Removed: lv_obj_set_style_align(LV_ALIGN_RIGHT_MID) — it fights with the
   * parent's flex layout and was part of the hang condition. text_align
   * alone gives the right visual effect. */
  return lbl;
}

/* Assistant bubble — solid ghost surface (not a 30% tint): reads as a card
 * against BB_UI_BG and keeps the transcript monochrome. */
static lv_obj_t* make_assistant_label(void) {
  lv_obj_t* lbl = make_msg_label(UI_AI_SURFACE, UI_TEXT_MAIN, LV_TEXT_ALIGN_LEFT, 0);
  if (lbl != NULL) lv_obj_set_style_bg_opa(lbl, LV_OPA_COVER, 0);
  return lbl;
}

static void scroll_to_bottom_unconditional(void) {
  if (s_transcript == NULL) return;
  uint32_t cnt = lv_obj_get_child_count(s_transcript);
  if (cnt == 0) return;
  lv_obj_t* last = lv_obj_get_child(s_transcript, cnt - 1);
  if (last != NULL) {
    lv_obj_scroll_to_view(last, LV_ANIM_OFF);
  }
}

/* Auto-scroll hook called from append_* paths. Honors reading mode: if the
 * user scrolled away, leave the viewport alone. */
static void follow_tail_if_active(void) {
  if (!s_follow_tail) return;
  scroll_to_bottom_unconditional();
}

lv_obj_t* bb_chat_transcript_create(lv_obj_t* parent, int width, int height_px,
                                    int y_offset) {
  if (parent == NULL || s_transcript != NULL) return s_transcript;

  s_transcript = lv_obj_create(parent);
  lv_obj_remove_style_all(s_transcript);
  lv_obj_set_size(s_transcript, width, height_px);
  lv_obj_align(s_transcript, LV_ALIGN_TOP_LEFT, 0, y_offset);
  lv_obj_set_style_bg_opa(s_transcript, LV_OPA_TRANSP, 0);
  lv_obj_set_flex_flow(s_transcript, LV_FLEX_FLOW_COLUMN);
  lv_obj_set_flex_align(s_transcript, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_START,
                        LV_FLEX_ALIGN_START);
  lv_obj_set_style_pad_left(s_transcript, MSG_HMARGIN, 0);
  lv_obj_set_style_pad_right(s_transcript, MSG_HMARGIN, 0);
  lv_obj_set_style_pad_top(s_transcript, 2, 0);
  lv_obj_set_style_pad_bottom(s_transcript, 2, 0);
  lv_obj_add_flag(s_transcript, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scroll_dir(s_transcript, LV_DIR_VER);
  lv_obj_set_scrollbar_mode(s_transcript, LV_SCROLLBAR_MODE_AUTO);

  s_active_assistant = NULL;
  /* Fresh transcript starts in follow mode. */
  s_follow_tail = 1;
  return s_transcript;
}

void bb_chat_transcript_destroy(void) {
  /* Only reset internal pointers. The parent's lv_obj_del cascades to child objects,
   * so do NOT lv_obj_del(s_transcript) here — it would double-free. */
  s_transcript = NULL;
  s_active_assistant = NULL;
  s_follow_tail = 1;
}

lv_obj_t* bb_chat_transcript_get_container(void) {
  return s_transcript;
}

void bb_chat_transcript_append_user(const char* text) {
  if (s_transcript == NULL || text == NULL) return;
  /* A new user message marks the end of the previous assistant turn.
   * Flush any pending streamed chunks into the cache as one finalized
   * message BEFORE we record the new user line — otherwise the cloud
   * path (which emits a single REPLY_DELTA with the whole reply, and no
   * TURN_END) would lose the assistant text on sleep/wake. */
  bb_chat_cache_finalize_assistant();
  lv_obj_t* lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  if (lbl == NULL) return;
  lv_label_set_text(lbl, text);
  s_active_assistant = NULL;
  bb_chat_cache_append_user(text);
  follow_tail_if_active();
}

void bb_chat_transcript_append_assistant_chunk(const char* delta) {
  if (s_transcript == NULL || delta == NULL) return;
  if (s_active_assistant == NULL) {
    s_active_assistant = make_assistant_label();
    if (s_active_assistant == NULL) return;
    lv_label_set_text(s_active_assistant, delta);
  } else {
    lv_label_ins_text(s_active_assistant, LV_LABEL_POS_LAST, delta);
  }
  bb_chat_cache_append_assistant_chunk(delta);
  follow_tail_if_active();
}

void bb_chat_transcript_append_tool_call(const char* tool, const char* hint) {
  if (s_transcript == NULL) return;
  bb_chat_cache_finalize_assistant();
  lv_obj_t* lbl = make_msg_label(UI_TEXT_DIM, UI_TOOL_FG, LV_TEXT_ALIGN_LEFT, 1);
  if (lbl == NULL) return;
  lv_obj_set_style_bg_opa(lbl, LV_OPA_10, 0);
  char buf[160];
  if (hint != NULL && hint[0] != '\0') {
    snprintf(buf, sizeof(buf), "[tool] %s: %s", tool != NULL ? tool : "tool", hint);
  } else {
    snprintf(buf, sizeof(buf), "[tool] %s", tool != NULL ? tool : "tool");
  }
  lv_label_set_text(lbl, buf);
  s_active_assistant = NULL;
  bb_chat_cache_append_tool(tool, hint);
  follow_tail_if_active();
}

void bb_chat_transcript_append_error(const char* msg) {
  if (s_transcript == NULL) return;
  bb_chat_cache_finalize_assistant();
  lv_obj_t* lbl = make_msg_label(UI_ERROR_FG, UI_ERROR_FG, LV_TEXT_ALIGN_LEFT, 0);
  if (lbl == NULL) return;
  lv_obj_set_style_bg_opa(lbl, LV_OPA_20, 0);
  lv_obj_set_style_text_color(lbl, lv_color_hex(UI_ERROR_FG), 0);
  char buf[200];
  snprintf(buf, sizeof(buf), "! %s", msg != NULL ? msg : "error");
  lv_label_set_text(lbl, buf);
  s_active_assistant = NULL;
  bb_chat_cache_append_error(msg);
  follow_tail_if_active();
}

/* Render a dim centered separator line with an HH:MM time label.
 * Used to mark session-segment boundaries in the history transcript.
 * No background, just a short dim text label — keeps the tiny screen
 * from getting too cluttered. */
static void make_segment_separator(const char* label, int prepend) {
  if (s_transcript == NULL) return;
  lv_obj_t* sep = lv_label_create(s_transcript);
  lv_label_set_long_mode(sep, LV_LABEL_LONG_MODE_DOTS);
  lv_obj_set_width(sep, 320 - 2 * MSG_HMARGIN);
  lv_obj_set_style_text_font(sep, font(), 0);
  lv_obj_set_style_text_color(sep, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_align(sep, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_style_text_opa(sep, LV_OPA_60, 0);
  lv_obj_set_style_pad_top(sep, 4, 0);
  lv_obj_set_style_pad_bottom(sep, 4, 0);
  lv_label_set_text(sep, label);
  if (prepend) lv_obj_move_to_index(sep, 0);
}

/* Format a Unix-ms timestamp into "HH:MM" local-ish display.
 * We skip real TZ handling on device — display as UTC offset by the
 * adapter's reported time which is already in the user's wall-clock. */
static void format_hhmm(int64_t ts_ms, char* out, size_t out_sz) {
  int64_t ts_sec = ts_ms / 1000;
  int sec_of_day = (int)(ts_sec % 86400);
  if (sec_of_day < 0) sec_of_day += 86400;
  int hh = sec_of_day / 3600;
  int mm = (sec_of_day % 3600) / 60;
  snprintf(out, out_sz, "── %02d:%02d ──", hh, mm);
}

void bb_chat_transcript_append_history(const char* role, const char* content,
                                       int64_t timestamp_ms) {
  if (s_transcript == NULL || role == NULL || content == NULL) return;

  /* Insert a segment separator when there's a meaningful time gap. */
  if (timestamp_ms > 0) {
    int need_sep = (s_history_last_ts_ms == 0) ||
                   (timestamp_ms - s_history_last_ts_ms > BB_HISTORY_SEGMENT_GAP_MS);
    if (need_sep) {
      char label[24];
      format_hhmm(timestamp_ms, label, sizeof(label));
      make_segment_separator(label, /*prepend=*/0);
    }
    s_history_last_ts_ms = timestamp_ms;
  }

  int is_user = strcmp(role, "user") == 0;
  lv_obj_t* lbl = is_user
      ? make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0)
      : make_assistant_label();
  if (lbl == NULL) return;
  lv_label_set_text(lbl, content);
  s_active_assistant = NULL;
}

void bb_chat_transcript_prepend_history(const char* role, const char* content,
                                        int64_t timestamp_ms) {
  if (s_transcript == NULL || role == NULL || content == NULL) return;
  int is_user = strcmp(role, "user") == 0;
  lv_obj_t* lbl = is_user
      ? make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0)
      : make_assistant_label();
  if (lbl == NULL) return;
  lv_label_set_text(lbl, content);
  lv_obj_move_to_index(lbl, 0);

  /* For prepended (paginate-earlier) messages, insert a separator BEFORE
   * this message when the gap to the next-older message warrants it.
   * We can only detect this on the caller side (which has the full ordered
   * array); here we just check whether the caller supplied a timestamp that
   * differs from the last-seen one enough to need a divider.
   * Strategy: separator goes at index 0 AFTER moving the bubble to 0,
   * then we move the separator in front of the bubble (index 0 again). */
  if (timestamp_ms > 0 && s_history_last_ts_ms > 0 &&
      s_history_last_ts_ms - timestamp_ms > BB_HISTORY_SEGMENT_GAP_MS) {
    char label[24];
    format_hhmm(s_history_last_ts_ms, label, sizeof(label));
    make_segment_separator(label, /*prepend=*/1);
  }
  if (timestamp_ms > 0) {
    s_history_last_ts_ms = timestamp_ms;
  }
}

void bb_chat_transcript_scroll(int lines) {
  if (s_transcript == NULL || lines == 0) return;
  int32_t step = lv_font_get_line_height(font()) * lines;
  lv_obj_scroll_by_bounded(s_transcript, 0, step, LV_ANIM_OFF);
  /* ADR-017 — track follow-tail latch.
   * UP gesture (lines < 0) always drops us into reading mode. DOWN that
   * lands at the very bottom restores auto-scroll so the user can rejoin
   * the live stream by holding DOWN. */
  if (lines < 0) {
    set_follow_tail(0);
  } else if (transcript_at_bottom()) {
    set_follow_tail(1);
  }
}

void bb_chat_transcript_scroll_to_bottom(void) {
  scroll_to_bottom_unconditional();
  set_follow_tail(1);
}

int bb_chat_transcript_is_following(void) {
  return s_follow_tail ? 1 : 0;
}

void bb_chat_transcript_resume_follow(void) {
  if (s_transcript == NULL) return;
  scroll_to_bottom_unconditional();
  set_follow_tail(1);
}

int bb_chat_transcript_is_at_top(void) {
  if (s_transcript == NULL) return 0;
  return lv_obj_get_scroll_top(s_transcript) <= 4 ? 1 : 0;
}

void bb_chat_transcript_clear(void) {
  if (s_transcript == NULL) return;
  lv_obj_clean(s_transcript);
  s_active_assistant = NULL;
  s_history_last_ts_ms = 0;
}

void bb_chat_transcript_finalize_assistant(void) {
  s_active_assistant = NULL;
  bb_chat_cache_finalize_assistant();
}
