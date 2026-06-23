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
#include <time.h>

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
/* ADR-028 §2.5.1 (撤回语义) — anchor to the most recent *live* user bubble
 * (set by append_user, NOT history replay). withdraw_last_turn deletes from
 * here to the tail. NULL when there is no live turn to withdraw; reset on
 * create/clear/destroy so it never dangles past a transcript rebuild. */
static lv_obj_t* s_last_user_bubble;
/* Issue #169 — TTS subtitle bar. Overlay label sitting just above (or over)
 * the transcript area; created lazily on first set_subtitle call.
 * Positioned at the bottom of the transcript zone so it doesn't obscure
 * ongoing history while still being prominent during playback. */
static lv_obj_t* s_subtitle_label;    /* NULL until first set_subtitle */
/* Transcript geometry captured at create time for subtitle positioning. */
static lv_obj_t* s_transcript_parent;
static int       s_transcript_y;
static int       s_transcript_h;

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

/* Forward decls — used by flush_pending_assistant()/set_follow_tail() which are
 * defined above their bodies. */
static lv_obj_t* make_assistant_label(void);
static void scroll_to_bottom_unconditional(void);

/* Deferred assistant text (issue: scroll jank while a reply streams). When the
 * user has scrolled up to read history during TTS output (s_follow_tail=0),
 * incoming reply chunks are buffered here instead of doing a per-chunk
 * lv_label_ins_text → full-column relayout that fights the user's scroll. The
 * buffer is flushed into the live label in ONE relayout when the user returns
 * to the tail or the turn ends, so the screen stays static ("静态齐屏") while
 * reading. The cache (bb_chat_cache) is always kept current, so no text is lost
 * even if this view buffer overflows (then we fall back to live append). */
#define BB_PENDING_ASSISTANT_CAP 4096
static char s_pending_assistant[BB_PENDING_ASSISTANT_CAP];
static size_t s_pending_assistant_len;

static void flush_pending_assistant(void) {
  if (s_pending_assistant_len == 0) return;
  if (s_active_assistant == NULL) {
    s_active_assistant = make_assistant_label();
    if (s_active_assistant != NULL) {
      lv_label_set_text(s_active_assistant, s_pending_assistant);
    }
  } else {
    lv_label_ins_text(s_active_assistant, LV_LABEL_POS_LAST, s_pending_assistant);
  }
  s_pending_assistant_len = 0;
  s_pending_assistant[0] = '\0';
}

static void set_follow_tail(int follow) {
  follow = follow ? 1 : 0;
  if (s_follow_tail == follow) return;
  s_follow_tail = follow;
  if (follow) {
    /* Re-joining the live tail: render any text deferred while reading, in one
     * relayout, then snap to bottom so the now-complete reply is visible. */
    flush_pending_assistant();
    scroll_to_bottom_unconditional();
  }
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
  /* Capture geometry for lazy subtitle creation (Issue #169). */
  s_transcript_parent = parent;
  s_transcript_y      = y_offset;
  s_transcript_h      = height_px;

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
  /* remove_style_all above stripped the default scrollbar style, leaving the
   * thumb invisible — the user got no position feedback when paging up/down.
   * Paint a slim rounded thumb so the scroll position is always legible. */
  lv_obj_set_style_bg_color(s_transcript, lv_color_hex(UI_TEXT_MAIN), LV_PART_SCROLLBAR);
  lv_obj_set_style_bg_opa(s_transcript, LV_OPA_60, LV_PART_SCROLLBAR);
  lv_obj_set_style_width(s_transcript, 4, LV_PART_SCROLLBAR);
  lv_obj_set_style_radius(s_transcript, 2, LV_PART_SCROLLBAR);
  lv_obj_set_style_pad_right(s_transcript, 1, LV_PART_SCROLLBAR);

  s_active_assistant = NULL;
  s_last_user_bubble = NULL;
  s_subtitle_label = NULL;
  /* Fresh transcript starts in follow mode. */
  s_follow_tail = 1;
  return s_transcript;
}

void bb_chat_transcript_destroy(void) {
  /* Only reset internal pointers. The parent's lv_obj_del cascades to child objects,
   * so do NOT lv_obj_del(s_transcript) here — it would double-free. */
  s_transcript = NULL;
  s_active_assistant = NULL;
  s_last_user_bubble = NULL;
  s_subtitle_label = NULL;
  s_transcript_parent = NULL;
  s_transcript_y = 0;
  s_transcript_h = 0;
  s_follow_tail = 1;
}

lv_obj_t* bb_chat_transcript_get_container(void) {
  return s_transcript;
}

void bb_chat_transcript_append_user(const char* text) {
  if (s_transcript == NULL || text == NULL) return;
  /* If the user was reading history while the previous reply streamed, render
   * its deferred tail into the (old) assistant bubble before we close the turn,
   * so it isn't lost/misplaced. */
  flush_pending_assistant();
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
  /* ADR-028 §2.5.1 — anchor the (new) live turn for a possible barge-in
   * withdraw. Only live user lines set this; history replay uses append_history. */
  s_last_user_bubble = lbl;
  bb_chat_cache_append_user(text);
  /* A new *live* user turn means the user is driving the conversation again.
   * Rejoin the live tail even if they had scrolled up to read history (which
   * latched s_follow_tail=0), so this message and the incoming reply both
   * auto-scroll into view. History replay uses append_history, not this path,
   * so forcing follow here only fires on a genuine new turn. */
  set_follow_tail(1);
  scroll_to_bottom_unconditional();
}

void bb_chat_transcript_append_assistant_chunk(const char* delta) {
  if (s_transcript == NULL || delta == NULL) return;
  /* Cache is always current (persistence) regardless of reading mode. */
  bb_chat_cache_append_assistant_chunk(delta);
  /* Reading mode (user scrolled up during output): buffer the delta instead of
   * relaying out the column on every chunk — that fight with the user's scroll
   * is the jank when flipping history while a reply streams. Flushed in one pass
   * on return-to-tail / turn end. Overflow falls back to live append (no loss). */
  if (!s_follow_tail) {
    size_t dl = strlen(delta);
    size_t room = (size_t)(BB_PENDING_ASSISTANT_CAP - 1) - s_pending_assistant_len;
    if (dl <= room) {
      memcpy(s_pending_assistant + s_pending_assistant_len, delta, dl);
      s_pending_assistant_len += dl;
      s_pending_assistant[s_pending_assistant_len] = '\0';
      return;
    }
    /* buffer full → render what's buffered first (keep order), then live-append
     * this delta. Rare: only for a >4KB reply read-deferred the whole time. */
    flush_pending_assistant();
  }
  if (s_active_assistant == NULL) {
    s_active_assistant = make_assistant_label();
    if (s_active_assistant == NULL) return;
    lv_label_set_text(s_active_assistant, delta);
  } else {
    lv_label_ins_text(s_active_assistant, LV_LABEL_POS_LAST, delta);
  }
  follow_tail_if_active();
}

void bb_chat_transcript_append_tool_call(const char* tool, const char* hint) {
  if (s_transcript == NULL) return;
  flush_pending_assistant();
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
  flush_pending_assistant();
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
  /* ts is already in the user's wall-clock (adapter-provided), so interpret it
   * as-is via gmtime_r (no extra TZ shift) to get the calendar date + time. */
  time_t ts_sec = (time_t)(ts_ms / 1000);
  struct tm tmv;
  gmtime_r(&ts_sec, &tmv);
  /* ASCII-only dashes: the box-drawing char U+2500 (──) is not in the CJK font
   * subset and rendered as garbage (乱码). Hyphen-minus is always present.
   * Show date (MM-DD) + time so long histories are easy to place in time. */
  /* %100 bounds each field to 2 digits (values are already valid) so the
   * compiler can prove no format truncation into the caller's 24-byte buffer. */
  snprintf(out, out_sz, "-- %02d-%02d %02d:%02d --", (tmv.tm_mon + 1) % 100,
           tmv.tm_mday % 100, tmv.tm_hour % 100, tmv.tm_min % 100);
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
  /* LV_ANIM_ON: animate the manual UP/DOWN scroll so the direction of travel
   * is perceptible. LV_ANIM_OFF jumped instantly, which on this small screen
   * read as a flicker with no clear up-vs-down cue. Auto-scroll (follow-tail)
   * still uses LV_ANIM_OFF so streaming replies snap to the bottom. */
  lv_obj_scroll_by_bounded(s_transcript, 0, step, LV_ANIM_ON);
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
  s_last_user_bubble = NULL;
  s_pending_assistant_len = 0;
  s_pending_assistant[0] = '\0';
  s_history_last_ts_ms = 0;
}

void bb_chat_transcript_withdraw_last_turn(void) {
  /* ADR-028 §2.5.1 (撤回语义) — PTT barge-in withdraws the cancelled turn:
   * delete the most recent live user bubble and every bubble that came after
   * it (the in-flight reply / tool / error of the cancelled turn), keeping
   * earlier completed turns. No-op when there is no live turn to withdraw. */
  if (s_transcript == NULL || s_last_user_bubble == NULL) return;
  /* Render any text deferred while reading so it isn't stranded mid-delete. */
  s_pending_assistant_len = 0;
  s_pending_assistant[0] = '\0';
  uint32_t start = lv_obj_get_index(s_last_user_bubble);
  uint32_t cnt = lv_obj_get_child_count(s_transcript);
  /* Delete from the tail backward so sibling indices stay valid as we go. */
  for (uint32_t i = cnt; i > start; --i) {
    lv_obj_t* child = lv_obj_get_child(s_transcript, i - 1);
    if (child != NULL) lv_obj_del(child);
  }
  s_last_user_bubble = NULL;
  s_active_assistant = NULL;
  /* Drop the same turn from the persistence cache so it doesn't reappear on
   * wake / history replay. */
  bb_chat_cache_drop_last_turn();
  follow_tail_if_active();
}

void bb_chat_transcript_finalize_assistant(void) {
  /* Reply complete: render any text deferred while the user read history, so the
   * finished bubble is whole before we close the streaming label. */
  flush_pending_assistant();
  s_active_assistant = NULL;
  bb_chat_cache_finalize_assistant();
}

/* ── Issue #169 — TTS subtitle bar ─────────────────────────────────────────
 *
 * The subtitle label is a child of the transcript's *parent* (the same
 * parent passed to bb_chat_transcript_create), not a child of the scrollable
 * transcript container.  That way it floats at a fixed position regardless of
 * scroll state and does not participate in the flex-column layout that causes
 * lv_label_ins_text / scroll-to-view to reflow the whole bubble list.
 *
 * Visual design (1.47" 172×320):
 *   - bottom of the transcript zone = y_offset + transcript_h − subtitle_h
 *   - full width minus the standard horizontal margin
 *   - semi-transparent ghost surface background (same token as assistant
 *     bubbles) so it reads as a card floating over the history
 *   - single-line DOT mode to avoid reflow hang (same rule as bubble labels)
 *   - hidden by default; bb_chat_transcript_set_subtitle shows it
 *
 * s_transcript_parent / s_transcript_y / s_transcript_h are declared at the
 * top of the file alongside the other statics. */

#define SUBTITLE_H       20   /* px — one line of text + MSG_PAD*2 */
#define SUBTITLE_Y_ABOVE 0    /* offset above transcript bottom edge */

static void ensure_subtitle_created(void) {
  if (s_subtitle_label != NULL) return;
  if (s_transcript_parent == NULL) return;

  s_subtitle_label = lv_label_create(s_transcript_parent);
  lv_label_set_long_mode(s_subtitle_label, LV_LABEL_LONG_MODE_DOTS);
  lv_obj_set_size(s_subtitle_label, 320 - 2 * MSG_HMARGIN, SUBTITLE_H);
  /* Anchor to bottom of transcript zone */
  int y = s_transcript_y + s_transcript_h - SUBTITLE_H - SUBTITLE_Y_ABOVE;
  lv_obj_set_pos(s_subtitle_label, MSG_HMARGIN, y);
  lv_obj_set_style_text_font(s_subtitle_label, font(), 0);
  lv_obj_set_style_text_color(s_subtitle_label, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_align(s_subtitle_label, LV_TEXT_ALIGN_LEFT, 0);
  lv_obj_set_style_bg_color(s_subtitle_label, lv_color_hex(UI_AI_SURFACE), 0);
  lv_obj_set_style_bg_opa(s_subtitle_label, LV_OPA_COVER, 0);
  lv_obj_set_style_radius(s_subtitle_label, MSG_RADIUS, 0);
  lv_obj_set_style_pad_all(s_subtitle_label, MSG_PAD, 0);
  /* Start hidden */
  lv_obj_add_flag(s_subtitle_label, LV_OBJ_FLAG_HIDDEN);
  /* Float above the transcript scrollable area */
  lv_obj_move_foreground(s_subtitle_label);
}

void bb_chat_transcript_set_subtitle(const char* text) {
  ensure_subtitle_created();
  if (s_subtitle_label == NULL) return;
  lv_label_set_text(s_subtitle_label, text != NULL ? text : "");
  lv_obj_clear_flag(s_subtitle_label, LV_OBJ_FLAG_HIDDEN);
  lv_obj_move_foreground(s_subtitle_label);
}

void bb_chat_transcript_clear_subtitle(void) {
  if (s_subtitle_label == NULL) return;
  lv_obj_add_flag(s_subtitle_label, LV_OBJ_FLAG_HIDDEN);
}
