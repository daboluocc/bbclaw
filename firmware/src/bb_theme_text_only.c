#include <stdio.h>
#include <string.h>

#include "bb_agent_theme.h"
#include "bb_ui_settings.h"
#include "bb_wifi.h"
#include "esp_log.h"
#include "lvgl.h"

static const char* TAG = "bb_agent_theme";

/*
 * 极简文本主题：上方一行状态条 + 下方滚动 transcript。
 * 颜色与 bb_lvgl_display.c 的调色板对齐（青绿黑系）。
 *
 * Transcript 渲染策略：
 *   - 用户消息 / 助手消息 / tool_call / error 各自一个 lv_label 子对象
 *   - 助手流式 chunk 持续 append 到"当前助手 label"上（s_active_assistant_lbl）
 *   - 收到 turn_end / 用户新消息 / 错误 / tool_call 时把 active 置 NULL，
 *     下一个 chunk 会新建一个 label。这样即避免每个 chunk 一个 label 撑爆对象数，
 *     也保留了"自然分段"。lv_label_ins_text 是 amortized O(n)，对短回复够用。
 */

/* 颜色（与 bb_lvgl_display.c 同款调色板） */
#define UI_SCR_BG      0x0a0e0c
#define UI_TEXT_MAIN   0xd8ebe4
#define UI_TEXT_DIM    0x7a9a8c
#define UI_STATUS_FG   0x8fbcac
#define UI_ME_ACCENT   0x2ec4a0  /* 用户气泡 */
#define UI_AI_ACCENT   0x4a9fd8  /* 助手气泡边角点缀（保留供后续） */
#define UI_TOOL_FG     0x9aa5a1
#define UI_ERROR_FG    0xe66f6f

/* 布局
 * 显示 320x172 (landscape). 不能用 lv_pct(100) - TOPBAR_H 这种表达式
 * （lv_pct 是特殊编码值不能减整数），改用绝对像素：
 *   middle = 172 - 18 - 20 = 134 px
 */
#define TOPBAR_H       18
#define INPUT_H        20
#define MIDDLE_H       (172 - TOPBAR_H - INPUT_H)
#define MSG_PAD        4
#define MSG_RADIUS     6
#define MSG_HMARGIN    8

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
static const lv_font_t* theme_font(void) { return &lv_font_bbclaw_cjk; }
#else
static const lv_font_t* theme_font(void) { return lv_font_get_default(); }
#endif

typedef struct {
  lv_obj_t* root;
  lv_obj_t* topbar_lbl;
  lv_obj_t* transcript;       /* 滚动容器，flex column */
  lv_obj_t* input_lbl;        /* placeholder 输入区 */
  lv_obj_t* active_assistant; /* 当前正在累积的助手 label，turn_end / 其它消息后清掉 */
  char driver_buf[24];
  char session_buf[16];
  bb_agent_state_t state;
  int built;
} bb_text_theme_state_t;

static bb_text_theme_state_t s_st = {0};

static const char* state_name(bb_agent_state_t s) {
  switch (s) {
    case BB_AGENT_STATE_SLEEP:     return "sleep";
    case BB_AGENT_STATE_IDLE:      return "idle";
    case BB_AGENT_STATE_BUSY:      return "busy";
    case BB_AGENT_STATE_ATTENTION: return "attn";
    case BB_AGENT_STATE_CELEBRATE: return "yay";
    case BB_AGENT_STATE_DIZZY:     return "dizzy";
    case BB_AGENT_STATE_HEART:     return "heart";
    case BB_AGENT_STATE_LISTENING: return "listen";
    case BB_AGENT_STATE_SPEAKING:  return "speak";
  }
  return "?";
}

static void refresh_topbar(void) {
  if (s_st.topbar_lbl == NULL) return;
  char buf[128];
  /* ASCII-only flag suffix for TTS toggle + WiFi connectivity. Long-mode is
   * DOTS on the topbar label, so overflow truncates with "..." rather than
   * wrapping. */
  snprintf(buf, sizeof(buf), "%s | %s | %s %s%s",
           s_st.driver_buf[0] != '\0' ? s_st.driver_buf : "(no driver)",
           s_st.session_buf[0] != '\0' ? s_st.session_buf : "--------",
           state_name(s_st.state),
           bb_ui_settings_tts_enabled() ? "T+" : "T-",
           bb_wifi_is_connected()       ? "W+" : "W-");
  lv_label_set_text(s_st.topbar_lbl, buf);
}

/* 滚到底部。LVGL 9 的 lv_obj_scroll_to_y 需要绝对坐标；用 lv_obj_scroll_to_view
 * 把最后一个孩子拽进视口最简单。 */
static void scroll_to_bottom(void) {
  if (s_st.transcript == NULL) return;
  uint32_t cnt = lv_obj_get_child_count(s_st.transcript);
  if (cnt == 0) return;
  lv_obj_t* last = lv_obj_get_child(s_st.transcript, cnt - 1);
  if (last != NULL) {
    lv_obj_scroll_to_view(last, LV_ANIM_OFF);
  }
}

/* Phase 4.9: manual scroll by N lines. lines<0 = up, lines>0 = down. */
static void theme_scroll_transcript(int lines) {
  if (s_st.transcript == NULL || lines == 0) return;
  int32_t step = lv_font_get_line_height(theme_font()) * lines;
  lv_obj_scroll_by_bounded(s_st.transcript, 0, step, LV_ANIM_OFF);
}

static lv_obj_t* make_msg_label(uint32_t bg_color, uint32_t fg_color, lv_text_align_t align,
                                int italic) {
  lv_obj_t* lbl = lv_label_create(s_st.transcript);
  lv_label_set_long_mode(lbl, LV_LABEL_LONG_MODE_WRAP);
  lv_obj_set_width(lbl, lv_pct(90));
  lv_obj_set_style_text_font(lbl, theme_font(), 0);
  lv_obj_set_style_text_color(lbl, lv_color_hex(fg_color), 0);
  lv_obj_set_style_text_align(lbl, align, 0);
  lv_obj_set_style_bg_color(lbl, lv_color_hex(bg_color), 0);
  lv_obj_set_style_bg_opa(lbl, LV_OPA_30, 0);
  lv_obj_set_style_radius(lbl, MSG_RADIUS, 0);
  lv_obj_set_style_pad_all(lbl, MSG_PAD, 0);
  lv_obj_set_style_margin_top(lbl, 2, 0);
  lv_obj_set_style_margin_bottom(lbl, 2, 0);
  if (italic) {
    /* LVGL 没有原生 italic flag；保留 hook，后续可换斜体字体 */
    lv_obj_set_style_text_opa(lbl, LV_OPA_70, 0);
  }
  if (align == LV_TEXT_ALIGN_RIGHT) {
    lv_obj_set_style_align(lbl, LV_ALIGN_RIGHT_MID, 0);
  }
  return lbl;
}

/* ── theme callbacks ── */

static void theme_on_enter(lv_obj_t* parent) {
  if (s_st.built) {
    ESP_LOGW(TAG, "text-only on_enter: already built");
    return;
  }
  if (parent == NULL) return;

  s_st.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_st.root);
  lv_obj_set_size(s_st.root, lv_pct(100), lv_pct(100));
  lv_obj_set_style_bg_color(s_st.root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_st.root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_st.root, LV_OBJ_FLAG_SCROLLABLE);

  /* Top status bar */
  s_st.topbar_lbl = lv_label_create(s_st.root);
  lv_obj_set_size(s_st.topbar_lbl, lv_pct(100), TOPBAR_H);
  lv_obj_align(s_st.topbar_lbl, LV_ALIGN_TOP_LEFT, 0, 0);
  lv_obj_set_style_text_font(s_st.topbar_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.topbar_lbl, lv_color_hex(UI_STATUS_FG), 0);
  lv_obj_set_style_text_align(s_st.topbar_lbl, LV_TEXT_ALIGN_LEFT, 0);
  lv_obj_set_style_pad_left(s_st.topbar_lbl, 4, 0);
  lv_obj_set_style_pad_right(s_st.topbar_lbl, 4, 0);
  lv_label_set_long_mode(s_st.topbar_lbl, LV_LABEL_LONG_MODE_DOTS);

  /* Transcript（中央可滚动）— 绝对像素高度，避免 lv_pct - int 的坑 */
  s_st.transcript = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.transcript);
  lv_obj_set_size(s_st.transcript, lv_pct(100), MIDDLE_H);
  lv_obj_align(s_st.transcript, LV_ALIGN_TOP_LEFT, 0, TOPBAR_H);
  lv_obj_set_style_bg_opa(s_st.transcript, LV_OPA_TRANSP, 0);
  lv_obj_set_flex_flow(s_st.transcript, LV_FLEX_FLOW_COLUMN);
  lv_obj_set_flex_align(s_st.transcript, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_START,
                        LV_FLEX_ALIGN_START);
  lv_obj_set_style_pad_left(s_st.transcript, MSG_HMARGIN, 0);
  lv_obj_set_style_pad_right(s_st.transcript, MSG_HMARGIN, 0);
  lv_obj_set_style_pad_top(s_st.transcript, 2, 0);
  lv_obj_set_style_pad_bottom(s_st.transcript, 2, 0);
  lv_obj_add_flag(s_st.transcript, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scroll_dir(s_st.transcript, LV_DIR_VER);
  lv_obj_set_scrollbar_mode(s_st.transcript, LV_SCROLLBAR_MODE_AUTO);

  /* Input placeholder */
  s_st.input_lbl = lv_label_create(s_st.root);
  lv_obj_set_size(s_st.input_lbl, lv_pct(100), INPUT_H);
  lv_obj_align(s_st.input_lbl, LV_ALIGN_BOTTOM_LEFT, 0, 0);
  lv_obj_set_style_text_font(s_st.input_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.input_lbl, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_pad_left(s_st.input_lbl, 4, 0);
  lv_obj_set_style_pad_right(s_st.input_lbl, 4, 0);
  lv_label_set_text(s_st.input_lbl, "(input not wired yet)");

  s_st.active_assistant = NULL;
  s_st.built = 1;
  refresh_topbar();
}

static void theme_on_exit(void) {
  if (!s_st.built) return;
  if (s_st.root != NULL) {
    lv_obj_del(s_st.root);
  }
  s_st.root = NULL;
  s_st.topbar_lbl = NULL;
  s_st.transcript = NULL;
  s_st.input_lbl = NULL;
  s_st.active_assistant = NULL;
  s_st.built = 0;
}

static void theme_set_state(bb_agent_state_t state) {
  s_st.state = state;
  if (!s_st.built) return;
  refresh_topbar();
}

static void theme_append_user(const char* text) {
  if (!s_st.built || text == NULL) return;
  lv_obj_t* lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  lv_label_set_text(lbl, text);
  s_st.active_assistant = NULL;
  scroll_to_bottom();
}

static void theme_append_assistant_chunk(const char* delta) {
  if (!s_st.built || delta == NULL) return;
  if (s_st.active_assistant == NULL) {
    s_st.active_assistant = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_LEFT, 0);
    lv_label_set_text(s_st.active_assistant, delta);
  } else {
    lv_label_ins_text(s_st.active_assistant, LV_LABEL_POS_LAST, delta);
  }
  scroll_to_bottom();
}

static void theme_append_tool_call(const char* tool, const char* hint) {
  if (!s_st.built) return;
  lv_obj_t* lbl = make_msg_label(UI_TEXT_DIM, UI_TOOL_FG, LV_TEXT_ALIGN_LEFT, 1);
  lv_obj_set_style_bg_opa(lbl, LV_OPA_10, 0);
  char buf[160];
  if (hint != NULL && hint[0] != '\0') {
    snprintf(buf, sizeof(buf), "\xE2\x9A\x99 %s: %s", tool != NULL ? tool : "tool", hint);
  } else {
    snprintf(buf, sizeof(buf), "\xE2\x9A\x99 %s", tool != NULL ? tool : "tool");
  }
  lv_label_set_text(lbl, buf);
  s_st.active_assistant = NULL;
  scroll_to_bottom();
}

static void theme_append_error(const char* msg) {
  if (!s_st.built) return;
  lv_obj_t* lbl = make_msg_label(UI_ERROR_FG, UI_ERROR_FG, LV_TEXT_ALIGN_LEFT, 0);
  lv_obj_set_style_bg_opa(lbl, LV_OPA_20, 0);
  lv_obj_set_style_text_color(lbl, lv_color_hex(UI_ERROR_FG), 0);
  char buf[200];
  snprintf(buf, sizeof(buf), "! %s", msg != NULL ? msg : "error");
  lv_label_set_text(lbl, buf);
  s_st.active_assistant = NULL;
  scroll_to_bottom();
}

static void theme_set_driver(const char* driver_name) {
  if (driver_name == NULL) return;
  strncpy(s_st.driver_buf, driver_name, sizeof(s_st.driver_buf) - 1);
  s_st.driver_buf[sizeof(s_st.driver_buf) - 1] = '\0';
  if (s_st.built) refresh_topbar();
}

static void theme_set_session(const char* sid_short) {
  if (sid_short == NULL) return;
  strncpy(s_st.session_buf, sid_short, sizeof(s_st.session_buf) - 1);
  s_st.session_buf[sizeof(s_st.session_buf) - 1] = '\0';
  if (s_st.built) refresh_topbar();
}

/* Phase S3 — history replay helpers.
 *
 * Append: render a finalized history message at the bottom of the transcript.
 * Crucially, this does NOT set s_st.active_assistant — otherwise an in-flight
 * streaming chunk would get appended onto the last historical assistant
 * message. Initial-load callers iterate chronologically and follow up with
 * scroll_to_bottom().
 *
 * Prepend: insert at index 0 of the flex column. Used for "scroll to top →
 * fetch earlier batch" lazy load. Caller iterates the new batch in REVERSE
 * order (newest in batch first) so the oldest in the batch ends up at the
 * very top after all the move_to_index(0) calls.
 */
static void theme_append_history_message(const char* role, const char* content) {
  if (!s_st.built || role == NULL || content == NULL) return;
  int is_user = strcmp(role, "user") == 0;
  lv_obj_t* lbl;
  if (is_user) {
    lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  } else {
    lbl = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_LEFT, 0);
  }
  lv_label_set_text(lbl, content);
  s_st.active_assistant = NULL;
}

static void theme_prepend_history_message(const char* role, const char* content) {
  if (!s_st.built || role == NULL || content == NULL) return;
  int is_user = strcmp(role, "user") == 0;
  lv_obj_t* lbl;
  if (is_user) {
    lbl = make_msg_label(UI_ME_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_RIGHT, 0);
  } else {
    lbl = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN, LV_TEXT_ALIGN_LEFT, 0);
  }
  lv_label_set_text(lbl, content);
  lv_obj_move_to_index(lbl, 0);
}

/* True iff the transcript is scrolled all the way to the top. Used by the
 * UI layer to detect "user wants earlier history" without having to expose
 * the LVGL handle through the theme API. */
static int theme_is_transcript_at_top(void) {
  if (!s_st.built || s_st.transcript == NULL) return 0;
  return lv_obj_get_scroll_top(s_st.transcript) <= 4 ? 1 : 0;
}

/* Public scroll-to-bottom hook. Wraps the existing private scroll_to_bottom
 * helper so the chat UI can reliably reset the viewport after a batch of
 * history appends — scroll_by_bounded only works once layout is settled,
 * scroll_to_view triggers layout internally, so it works mid-frame. */
static void theme_scroll_transcript_to_bottom(void) {
  scroll_to_bottom();
}

/* ── 主题对象 + constructor 注册 ── */

static const bb_agent_theme_t s_text_only_theme = {
    .name = "text-only",
    .on_enter = theme_on_enter,
    .on_exit = theme_on_exit,
    .set_state = theme_set_state,
    .append_user = theme_append_user,
    .append_assistant_chunk = theme_append_assistant_chunk,
    .append_tool_call = theme_append_tool_call,
    .append_error = theme_append_error,
    .set_driver = theme_set_driver,
    .set_session = theme_set_session,
    .scroll_transcript = theme_scroll_transcript,
    .append_history_message = theme_append_history_message,
    .prepend_history_message = theme_prepend_history_message,
    .is_transcript_at_top = theme_is_transcript_at_top,
    .scroll_transcript_to_bottom = theme_scroll_transcript_to_bottom,
};

/*
 * 之前用 GCC __attribute__((constructor)) 自注册，但 ESP-IDF 静态库链接 +
 * --gc-sections 经常会把没有外部引用的构造函数 DCE 掉（实测面包板真机上 dispatcher
 * 起来后 registry 是空的）。改成显式 init —— bb_radio_app_start 启动时调一次。
 */
void bb_theme_text_only_init(void) {
  bb_agent_theme_register(&s_text_only_theme);
}
