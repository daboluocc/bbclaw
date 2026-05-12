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

#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

/* Color palette — matches buddy-anim */
#define UI_TEXT_MAIN   0xd8ebe4
#define UI_TEXT_DIM    0x7a9a8c
#define UI_ME_ACCENT   0x2ec4a0
#define UI_AI_ACCENT   0x4a9fd8
#define UI_TOOL_FG     0x9aa5a1
#define UI_ERROR_FG    0xe66f6f

#define MSG_PAD     4
#define MSG_RADIUS  6
#define MSG_HMARGIN 8

static lv_obj_t* s_transcript;
static lv_obj_t* s_active_assistant;  /* current streaming bubble, NULL after finalize */

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

static void scroll_to_bottom(void) {
  if (s_transcript == NULL) return;
  uint32_t cnt = lv_obj_get_child_count(s_transcript);
  if (cnt == 0) return;
  lv_obj_t* last = lv_obj_get_child(s_transcript, cnt - 1);
  if (last != NULL) {
    lv_obj_scroll_to_view(last, LV_ANIM_OFF);
  }
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
  return s_transcript;
}

void bb_chat_transcript_destroy(void) {
  /* Only reset internal pointers. The parent's lv_obj_del cascades to child objects,
   * so do NOT lv_obj_del(s_transcript) here — it would double-free. */
  s_transcript = NULL;
  s_active_assistant = NULL;
}

lv_obj_t* bb_chat_transcript_get_container(void) {
  return s_transcript;
}

void bb_chat_transcript_append_user(const char* text) {
  if (s_transcript == NULL || text == NULL) return;
  lv_obj_t* lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  if (lbl == NULL) return;
  lv_label_set_text(lbl, text);
  s_active_assistant = NULL;
  scroll_to_bottom();
}

void bb_chat_transcript_append_assistant_chunk(const char* delta) {
  if (s_transcript == NULL || delta == NULL) return;
  if (s_active_assistant == NULL) {
    s_active_assistant = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN,
                                        LV_TEXT_ALIGN_LEFT, 0);
    if (s_active_assistant == NULL) return;
    lv_label_set_text(s_active_assistant, delta);
  } else {
    lv_label_ins_text(s_active_assistant, LV_LABEL_POS_LAST, delta);
  }
  scroll_to_bottom();
}

void bb_chat_transcript_append_tool_call(const char* tool, const char* hint) {
  if (s_transcript == NULL) return;
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
  scroll_to_bottom();
}

void bb_chat_transcript_append_error(const char* msg) {
  if (s_transcript == NULL) return;
  lv_obj_t* lbl = make_msg_label(UI_ERROR_FG, UI_ERROR_FG, LV_TEXT_ALIGN_LEFT, 0);
  if (lbl == NULL) return;
  lv_obj_set_style_bg_opa(lbl, LV_OPA_20, 0);
  lv_obj_set_style_text_color(lbl, lv_color_hex(UI_ERROR_FG), 0);
  char buf[200];
  snprintf(buf, sizeof(buf), "! %s", msg != NULL ? msg : "error");
  lv_label_set_text(lbl, buf);
  s_active_assistant = NULL;
  scroll_to_bottom();
}

void bb_chat_transcript_append_history(const char* role, const char* content) {
  if (s_transcript == NULL || role == NULL || content == NULL) return;
  int is_user = strcmp(role, "user") == 0;
  lv_obj_t* lbl;
  if (is_user) {
    lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  } else {
    lbl = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_LEFT, 0);
  }
  if (lbl == NULL) return;
  lv_label_set_text(lbl, content);
  s_active_assistant = NULL;
}

void bb_chat_transcript_prepend_history(const char* role, const char* content) {
  if (s_transcript == NULL || role == NULL || content == NULL) return;
  int is_user = strcmp(role, "user") == 0;
  lv_obj_t* lbl;
  if (is_user) {
    lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  } else {
    lbl = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_LEFT, 0);
  }
  if (lbl == NULL) return;
  lv_label_set_text(lbl, content);
  lv_obj_move_to_index(lbl, 0);
}

void bb_chat_transcript_scroll(int lines) {
  if (s_transcript == NULL || lines == 0) return;
  int32_t step = lv_font_get_line_height(font()) * lines;
  lv_obj_scroll_by_bounded(s_transcript, 0, step, LV_ANIM_OFF);
}

void bb_chat_transcript_scroll_to_bottom(void) {
  scroll_to_bottom();
}

int bb_chat_transcript_is_at_top(void) {
  if (s_transcript == NULL) return 0;
  return lv_obj_get_scroll_top(s_transcript) <= 4 ? 1 : 0;
}

void bb_chat_transcript_finalize_assistant(void) {
  s_active_assistant = NULL;
}
