#include <stdio.h>
#include <string.h>

#include "bb_agent_theme.h"
#include "bb_ui_settings.h"
#include "bb_wifi.h"
#include "esp_log.h"
#include "lvgl.h"

static const char* TAG = "bb_theme_buddy";

/*
 * buddy-ascii 主题（Phase 4.6）
 *
 * 灵感来自 claude-desktop-buddy 的"七态角色 + 表情/状态"渲染范式。
 * 这里用纯 ASCII（兼容 LVGL 默认 Montserrat 字体，不依赖 CJK 字库）
 * 表达 7 个状态，让设备有"性格"。
 *
 * 屏幕布局（320x172）：
 *   ┌──────────────────────────────────────────┐
 *   │ topbar: <driver> | <session> | <state>   │ 18px
 *   ├────────────────────────────┬─────────────┤
 *   │                            │             │
 *   │   transcript               │   buddy     │
 *   │   (scrolling, ~200px wide) │   (~110px)  │
 *   │                            │             │
 *   ├────────────────────────────┴─────────────┤
 *   │ input placeholder                        │ 20px
 *   └──────────────────────────────────────────┘
 *
 * Buddy 区是两个 label 垂直堆叠：
 *   - face_lbl: 上方表情（如 "(^_^)"）
 *   - mood_lbl: 下方状态短语（如 "thinking…"）
 *
 * 由 set_state() 切换；其它 callback（消息流）和 text-only 主题完全等价。
 */

/* 颜色（与 text-only / lvgl_display 同款） */
#define UI_SCR_BG      0x0a0e0c
#define UI_TEXT_MAIN   0xd8ebe4
#define UI_TEXT_DIM    0x7a9a8c
#define UI_STATUS_FG   0x8fbcac
#define UI_ME_ACCENT   0x2ec4a0
#define UI_AI_ACCENT   0x4a9fd8
#define UI_TOOL_FG     0x9aa5a1
#define UI_ERROR_FG    0xe66f6f
#define UI_BUDDY_FG    0xf0e6c8  /* 暖色调突出角色 */
#define UI_BUDDY_DIM   0xa49a83

/* 布局
 *
 * 显示是 320x172 (landscape, 来自 BBCLAW_ST7789_WIDTH/HEIGHT). 我们用绝对像素
 * 布局 buddy_panel 和 transcript —— LVGL 的 lv_pct() 是特殊编码值，不能跟整数
 * 做加减（lv_pct(100) - TOPBAR_H 会算出垃圾尺寸），导致 buddy 角色看不到。
 * 这里直接算好：
 *   topbar      = 18 px
 *   input       = 20 px
 *   middle area = 172 - 18 - 20 = 134 px (= MIDDLE_H)
 *   buddy panel = 110 x 134 (右栏)
 *   transcript  = 210 x 134 (左栏 = 320 - 110)
 */
#define TOPBAR_H       18
#define INPUT_H        20
#define BUDDY_W        110
#define MIDDLE_H       (172 - TOPBAR_H - INPUT_H)
#define TRANSCRIPT_W   (320 - BUDDY_W)
#define MSG_PAD        4
#define MSG_RADIUS     6
#define MSG_HMARGIN    6

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
static const lv_font_t* theme_font(void) { return &lv_font_bbclaw_cjk; }
#else
static const lv_font_t* theme_font(void) { return lv_font_get_default(); }
#endif

typedef struct {
  lv_obj_t* root;
  lv_obj_t* topbar_lbl;
  lv_obj_t* transcript;
  lv_obj_t* input_lbl;
  lv_obj_t* buddy_panel;
  lv_obj_t* face_lbl;
  lv_obj_t* mood_lbl;
  lv_obj_t* active_assistant;
  char driver_buf[24];
  char session_buf[16];
  bb_agent_state_t state;
  int built;
} bb_buddy_state_t;

static bb_buddy_state_t s_st = {0};

static const char* state_short(bb_agent_state_t s) {
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

/* 七态表情 + 副标题。两行布局，每个状态独立。
 * 注意：用 ASCII（含基本标点 + 空格 + < > / 等），LVGL 默认 Montserrat 全部支持。
 * 长度控制在 ~12 字符内，能塞进 BUDDY_W 宽度（默认字体 ~7px/字符）。 */
typedef struct {
  const char* face;
  const char* mood;
} buddy_glyph_t;

static const buddy_glyph_t k_glyphs[] = {
    [BB_AGENT_STATE_SLEEP]     = {.face = "(-_-)",   .mood = "zzz..."},
    [BB_AGENT_STATE_IDLE]      = {.face = "(^_^)",   .mood = "ready"},
    [BB_AGENT_STATE_BUSY]      = {.face = "(o_o)",   .mood = "thinking..."},
    [BB_AGENT_STATE_ATTENTION] = {.face = "(O_O)?",  .mood = "your turn"},
    [BB_AGENT_STATE_CELEBRATE] = {.face = "\\(^o^)/", .mood = "yay!"},
    [BB_AGENT_STATE_DIZZY]     = {.face = "(X_X)",   .mood = "oops..."},
    [BB_AGENT_STATE_HEART]     = {.face = "(^_^)",   .mood = "<3"},
    [BB_AGENT_STATE_LISTENING] = {.face = "(o.o)\"",  .mood = "listening..."},
    [BB_AGENT_STATE_SPEAKING]  = {.face = "(^o^)~",  .mood = "speaking..."},
};

#define K_GLYPH_COUNT ((int)(sizeof(k_glyphs) / sizeof(k_glyphs[0])))

static void refresh_topbar(void) {
  if (s_st.topbar_lbl == NULL) return;
  char buf[128];
  /* ASCII-only flag suffix for TTS toggle + WiFi connectivity. The buddy
   * topbar spans the full screen width (the right column is the buddy
   * panel below TOPBAR_H, not beside it), so the layout matches text-only.
   * Long-mode DOTS guarantees graceful truncation if a driver name is
   * unusually long. */
  snprintf(buf, sizeof(buf), "%s | %s | %s %s%s",
           s_st.driver_buf[0] != '\0' ? s_st.driver_buf : "(no driver)",
           s_st.session_buf[0] != '\0' ? s_st.session_buf : "--------",
           state_short(s_st.state),
           bb_ui_settings_tts_enabled() ? "T+" : "T-",
           bb_wifi_is_connected()       ? "W+" : "W-");
  lv_label_set_text(s_st.topbar_lbl, buf);
}

/* ADR-009 appendix: for the "neutral" states (IDLE / BUSY / HEART) the face
 * varies by active driver, so the device subtly signals who's answering.
 * LISTENING / SPEAKING / DIZZY / CELEBRATE / ATTENTION / SLEEP convey
 * action/emotion semantics and keep their distinctive faces regardless of
 * driver. The mood text is always the state-defined mood. */
static const char* per_driver_face(const char* driver_name) {
  if (driver_name == NULL || driver_name[0] == '\0') return "(^_^)";
  if (strcmp(driver_name, "claude-code") == 0) return "(^_^)";
  if (strcmp(driver_name, "openclaw")    == 0) return "(O_O)";
  if (strcmp(driver_name, "ollama")      == 0) return "(v_v)";
  if (strcmp(driver_name, "opencode")    == 0) return "(._.)";
  return "(^_^)";  /* fallback to claude face for unknown drivers */
}

static void refresh_buddy(void) {
  if (s_st.face_lbl == NULL || s_st.mood_lbl == NULL) return;
  int idx = (int)s_st.state;
  if (idx < 0 || idx >= K_GLYPH_COUNT) idx = BB_AGENT_STATE_IDLE;
  const buddy_glyph_t* g = &k_glyphs[idx];
  const char* face = g->face;
  if (idx == BB_AGENT_STATE_IDLE || idx == BB_AGENT_STATE_BUSY ||
      idx == BB_AGENT_STATE_HEART) {
    face = per_driver_face(s_st.driver_buf);
  }
  lv_label_set_text(s_st.face_lbl, face);
  lv_label_set_text(s_st.mood_lbl, g->mood);
}

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
  lv_obj_set_width(lbl, lv_pct(95));
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
    ESP_LOGW(TAG, "buddy-ascii on_enter: already built");
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

  /* Buddy panel (right column). Absolute pixel size + absolute label
   * positions (no flex) — see the layout block at the top of this file
   * for why we don't use lv_pct arithmetic. */
  ESP_LOGI(TAG, "buddy on_enter: building panel %dx%d at right", BUDDY_W, MIDDLE_H);
  s_st.buddy_panel = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.buddy_panel);
  lv_obj_set_size(s_st.buddy_panel, BUDDY_W, MIDDLE_H);
  lv_obj_align(s_st.buddy_panel, LV_ALIGN_TOP_RIGHT, 0, TOPBAR_H);
  lv_obj_set_style_bg_color(s_st.buddy_panel, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_st.buddy_panel, LV_OPA_COVER, 0);
  /* Subtle 1-px left border to visually separate from transcript. */
  lv_obj_set_style_border_color(s_st.buddy_panel, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_border_width(s_st.buddy_panel, 1, 0);
  lv_obj_set_style_border_side(s_st.buddy_panel, LV_BORDER_SIDE_LEFT, 0);
  lv_obj_set_style_border_opa(s_st.buddy_panel, LV_OPA_50, 0);
  lv_obj_clear_flag(s_st.buddy_panel, LV_OBJ_FLAG_SCROLLABLE);

  /* Face label — explicit position, big enough to read at arm's length.
   * Centered horizontally inside the 110-px panel, vertically at the top
   * third (so picker can't easily occlude it). */
  s_st.face_lbl = lv_label_create(s_st.buddy_panel);
  lv_obj_set_size(s_st.face_lbl, BUDDY_W - 8, 28);
  lv_obj_align(s_st.face_lbl, LV_ALIGN_TOP_MID, 0, 30);
  lv_obj_set_style_text_font(s_st.face_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.face_lbl, lv_color_hex(UI_BUDDY_FG), 0);
  lv_obj_set_style_text_align(s_st.face_lbl, LV_TEXT_ALIGN_CENTER, 0);

  /* Mood label — sits below face. */
  s_st.mood_lbl = lv_label_create(s_st.buddy_panel);
  lv_obj_set_size(s_st.mood_lbl, BUDDY_W - 8, 24);
  lv_obj_align(s_st.mood_lbl, LV_ALIGN_TOP_MID, 0, 64);
  lv_obj_set_style_text_font(s_st.mood_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.mood_lbl, lv_color_hex(UI_BUDDY_DIM), 0);
  lv_obj_set_style_text_align(s_st.mood_lbl, LV_TEXT_ALIGN_CENTER, 0);

  /* Transcript (left column). Absolute pixel size — see header comment. */
  s_st.transcript = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.transcript);
  lv_obj_set_size(s_st.transcript, TRANSCRIPT_W, MIDDLE_H);
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
  refresh_buddy();
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
  s_st.buddy_panel = NULL;
  s_st.face_lbl = NULL;
  s_st.mood_lbl = NULL;
  s_st.active_assistant = NULL;
  s_st.built = 0;
}

static void theme_set_state(bb_agent_state_t state) {
  s_st.state = state;
  if (!s_st.built) return;
  refresh_topbar();
  refresh_buddy();
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
    /* "tool: hint" — keep ASCII so we don't depend on the gear codepoint
     * which Montserrat doesn't carry by default. */
    snprintf(buf, sizeof(buf), "[tool] %s: %s", tool != NULL ? tool : "tool", hint);
  } else {
    snprintf(buf, sizeof(buf), "[tool] %s", tool != NULL ? tool : "tool");
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
  if (s_st.built) {
    refresh_topbar();
    /* Driver change can flip the IDLE/BUSY/HEART face per ADR-009. */
    refresh_buddy();
  }
}

static void theme_set_session(const char* sid_short) {
  if (sid_short == NULL) return;
  strncpy(s_st.session_buf, sid_short, sizeof(s_st.session_buf) - 1);
  s_st.session_buf[sizeof(s_st.session_buf) - 1] = '\0';
  if (s_st.built) refresh_topbar();
}

static const bb_agent_theme_t s_buddy_ascii_theme = {
    .name = "buddy-ascii",
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
};

/* 显式 init（与 text-only 同样的 DCE-defeating 模式）。
 * bb_radio_app_start 启动时调一次。 */
void bb_theme_buddy_ascii_init(void) {
  bb_agent_theme_register(&s_buddy_ascii_theme);
}
