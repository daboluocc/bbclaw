#include <stdio.h>
#include <string.h>

#include "bb_agent_theme.h"
#include "bb_lvgl_assets.h"
#include "bb_lvgl_element_assets.h"
#include "bb_power.h"
#include "bb_ui_settings.h"
#include "bb_wifi.h"
#include "esp_log.h"
#include "lvgl.h"

static const char* TAG = "bb_theme_anim";

/*
 * buddy-anim — the single shipping agent-chat theme.
 *
 * 布局：topbar（状态 + buddy face/mood 右侧）/ 全宽滚动 transcript。每个九态
 * （sleep/idle/busy/attention/celebrate/dizzy/heart/listening/speaking）附一个
 * LVGL 动效，靠 lv_anim 驱动 obj 属性（透明度/位置/缩放/颜色）让角色"动起来"，
 * 不依赖美术资产。Buddy 动画限制在 topbar 右侧 ~86×20 区域内。
 *
 * 历史回放（Phase S3）：transcript 是 LVGL flex column；append_history_message
 * 在末尾追加完成的 user/assistant label，prepend_history_message 把更老的批次
 * 插到顶部（lv_obj_move_to_index(0)）实现"上翻自动加载"。
 */

#define UI_SCR_BG      0x0a0e0c
#define UI_TEXT_MAIN   0xd8ebe4
#define UI_TEXT_DIM    0x7a9a8c
#define UI_STATUS_FG   0x8fbcac
#define UI_ME_ACCENT   0x2ec4a0
#define UI_AI_ACCENT   0x4a9fd8
#define UI_TOOL_FG     0x9aa5a1
#define UI_ERROR_FG    0xe66f6f
#define UI_BUDDY_FG    0xf0e6c8
#define UI_BUDDY_DIM   0xa49a83
#define UI_ATTN_FG     0xffd166
#define UI_CELEB_FG    0xff8fd0

/* Screen corner inset — prevents content from being clipped by the physical
 * display's rounded corners (~R12-R16 on the 1.47" ST7789 panel). */
#define SCREEN_CORNER_INSET_X  8
#define SCREEN_CORNER_INSET_Y  4

#define TOPBAR_H       20
#define BUDDY_TOPBAR_W 86   /* fixed width for buddy face+mood in topbar */
#define TOPBAR_GAP     3
#define MIDDLE_H       (172 - TOPBAR_H - SCREEN_CORNER_INSET_Y - TOPBAR_GAP)
#define MSG_PAD        4
#define MSG_RADIUS     6
#define MSG_HMARGIN    SCREEN_CORNER_INSET_X

/* Face label home position (relative to topbar buddy container). Animations
 * transform around this baseline so we always have a stable resting pose.
 * The container is ~86x20, so keep amplitudes small. */
#define FACE_X0        0
#define FACE_Y0        2
#define MOOD_X0        0
#define MOOD_Y0        2

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
static const lv_font_t* theme_font(void) { return &lv_font_bbclaw_cjk; }
#else
static const lv_font_t* theme_font(void) { return lv_font_get_default(); }
#endif

typedef struct {
  lv_obj_t* root;
  /* topbar */
  lv_obj_t* topbar;
  lv_obj_t* topbar_icon;
  lv_obj_t* topbar_driver_lbl;
  lv_obj_t* topbar_session_lbl;
  lv_obj_t* topbar_bat_container;
  lv_obj_t* topbar_bat_fill;
  lv_obj_t* topbar_bat_frame;
  lv_obj_t* topbar_bat_lbl;
  /* buddy (lives inside topbar, right side) */
  lv_obj_t* topbar_buddy;   /* container for face + mood in topbar */
  lv_obj_t* face_lbl;
  lv_obj_t* mood_lbl;
  /* main */
  lv_obj_t* transcript;
  lv_obj_t* active_assistant;
  lv_timer_t* dots_timer;
  int dots_phase;
  char driver_buf[24];
  char session_buf[16];
  bb_agent_state_t state;
  int built;
} bb_anim_state_t;

static bb_anim_state_t s_st = {0};

typedef struct {
  const char* face;
  const char* mood;
} buddy_glyph_t;

static const buddy_glyph_t k_glyphs[] = {
    [BB_AGENT_STATE_SLEEP]     = {.face = "(-_-)",   .mood = "zzz..."},
    [BB_AGENT_STATE_IDLE]      = {.face = "(^_^)",   .mood = "ready"},
    [BB_AGENT_STATE_BUSY]      = {.face = "(o_o)",   .mood = "thinking"},
    [BB_AGENT_STATE_ATTENTION] = {.face = "(O_O)?",  .mood = "your turn"},
    [BB_AGENT_STATE_CELEBRATE] = {.face = "\\(^o^)/", .mood = "yay!"},
    [BB_AGENT_STATE_DIZZY]     = {.face = "(X_X)",   .mood = "oops..."},
    [BB_AGENT_STATE_HEART]     = {.face = "(^_^)",   .mood = "<3"},
    [BB_AGENT_STATE_LISTENING] = {.face = "(o.o)\"",  .mood = "listening..."},
    [BB_AGENT_STATE_SPEAKING]  = {.face = "(^o^)~",  .mood = "speaking..."},
};

#define K_GLYPH_COUNT ((int)(sizeof(k_glyphs) / sizeof(k_glyphs[0])))

/* ── animation exec callbacks ── */

static void anim_y_cb(void* obj, int32_t v) {
  lv_obj_set_y((lv_obj_t*)obj, (lv_coord_t)v);
}

static void anim_x_cb(void* obj, int32_t v) {
  lv_obj_set_x((lv_obj_t*)obj, (lv_coord_t)v);
}

static void anim_opa_cb(void* obj, int32_t v) {
  lv_obj_set_style_opa((lv_obj_t*)obj, (lv_opa_t)v, 0);
}

static void anim_zoom_cb(void* obj, int32_t v) {
  /* Style transform_scale: 256 = 1.0x. */
  lv_obj_set_style_transform_scale((lv_obj_t*)obj, (int32_t)v, 0);
}

static void anim_color_cb(void* obj, int32_t v) {
  /* v is interpreted as 0..255 lerp between BUDDY_FG and ATTN_FG. */
  uint8_t t = (uint8_t)v;
  uint32_t a = UI_BUDDY_FG;
  uint32_t b = UI_ATTN_FG;
  uint8_t ar = (a >> 16) & 0xff, ag = (a >> 8) & 0xff, ab = a & 0xff;
  uint8_t br = (b >> 16) & 0xff, bg = (b >> 8) & 0xff, bb = b & 0xff;
  uint8_t r = ar + (((int)br - (int)ar) * t) / 255;
  uint8_t g = ag + (((int)bg - (int)ag) * t) / 255;
  uint8_t bl = ab + (((int)bb - (int)ab) * t) / 255;
  lv_obj_set_style_text_color((lv_obj_t*)obj,
                              lv_color_make(r, g, bl), 0);
}

/* ── stop / reset animations ── */

static void stop_dots_timer(void) {
  if (s_st.dots_timer != NULL) {
    lv_timer_del(s_st.dots_timer);
    s_st.dots_timer = NULL;
  }
  s_st.dots_phase = 0;
}

static void stop_face_anims(void) {
  if (s_st.face_lbl == NULL) return;
  lv_anim_delete(s_st.face_lbl, anim_y_cb);
  lv_anim_delete(s_st.face_lbl, anim_x_cb);
  lv_anim_delete(s_st.face_lbl, anim_opa_cb);
  lv_anim_delete(s_st.face_lbl, anim_zoom_cb);
  lv_anim_delete(s_st.face_lbl, anim_color_cb);
  /* Reset to baseline so the next state's anim starts from a known pose. */
  lv_obj_set_pos(s_st.face_lbl, FACE_X0, FACE_Y0);
  lv_obj_set_style_opa(s_st.face_lbl, LV_OPA_COVER, 0);
  lv_obj_set_style_transform_scale(s_st.face_lbl, 256, 0);
  lv_obj_set_style_text_color(s_st.face_lbl, lv_color_hex(UI_BUDDY_FG), 0);
}

static void stop_mood_anims(void) {
  if (s_st.mood_lbl == NULL) return;
  lv_anim_delete(s_st.mood_lbl, anim_opa_cb);
  lv_anim_delete(s_st.mood_lbl, anim_y_cb);
  lv_obj_set_pos(s_st.mood_lbl, MOOD_X0, MOOD_Y0);
  lv_obj_set_style_opa(s_st.mood_lbl, LV_OPA_COVER, 0);
  lv_obj_set_style_text_color(s_st.mood_lbl, lv_color_hex(UI_BUDDY_DIM), 0);
}

static void stop_all_anims(void) {
  stop_dots_timer();
  stop_face_anims();
  stop_mood_anims();
}

/* ── animation builders ── */

static void start_pulse_opa(lv_obj_t* obj, lv_opa_t lo, lv_opa_t hi,
                            uint32_t dur) {
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, obj);
  lv_anim_set_values(&a, lo, hi);
  lv_anim_set_duration(&a, dur);
  lv_anim_set_playback_duration(&a, dur);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, anim_opa_cb);
  lv_anim_start(&a);
}

static void start_bob_y(lv_obj_t* obj, int32_t y0, int32_t amp, uint32_t dur) {
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, obj);
  lv_anim_set_values(&a, y0, y0 - amp);
  lv_anim_set_duration(&a, dur);
  lv_anim_set_playback_duration(&a, dur);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, anim_y_cb);
  lv_anim_start(&a);
}

static void start_sway_x(lv_obj_t* obj, int32_t x0, int32_t amp, uint32_t dur) {
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, obj);
  lv_anim_set_values(&a, x0 - amp, x0 + amp);
  lv_anim_set_duration(&a, dur);
  lv_anim_set_playback_duration(&a, dur);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, anim_x_cb);
  lv_anim_start(&a);
}

static void start_pulse_zoom(lv_obj_t* obj, int32_t lo, int32_t hi,
                             uint32_t dur) {
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, obj);
  lv_anim_set_values(&a, lo, hi);
  lv_anim_set_duration(&a, dur);
  lv_anim_set_playback_duration(&a, dur);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, anim_zoom_cb);
  lv_anim_start(&a);
}

static void start_color_blink(lv_obj_t* obj, uint32_t dur) {
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, obj);
  lv_anim_set_values(&a, 0, 255);
  lv_anim_set_duration(&a, dur);
  lv_anim_set_playback_duration(&a, dur);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, anim_color_cb);
  lv_anim_start(&a);
}

static void start_shake_x(lv_obj_t* obj, int32_t x0, uint32_t dur) {
  /* Asymmetric: quick burst to one side then back. Used for DIZZY. */
  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, obj);
  lv_anim_set_values(&a, x0 - 4, x0 + 4);
  lv_anim_set_duration(&a, dur);
  lv_anim_set_playback_duration(&a, dur);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_linear);
  lv_anim_set_exec_cb(&a, anim_x_cb);
  lv_anim_start(&a);
}

/* BUSY: cycle "thinking" / "thinking." / "thinking.." / "thinking..." */
static void busy_dots_timer_cb(lv_timer_t* t) {
  (void)t;
  if (s_st.mood_lbl == NULL) return;
  s_st.dots_phase = (s_st.dots_phase + 1) % 4;
  static const char* const k_dots[] = {"thinking", "thinking.",
                                       "thinking..", "thinking..."};
  lv_label_set_text(s_st.mood_lbl, k_dots[s_st.dots_phase]);
}

/* ── topbar / face refresh (shared with ascii) ── */

static const lv_image_dsc_t* state_icon(bb_agent_state_t s) {
  switch (s) {
    case BB_AGENT_STATE_BUSY:
    case BB_AGENT_STATE_LISTENING:
    case BB_AGENT_STATE_SPEAKING:
      return &bb_img_task;
    case BB_AGENT_STATE_DIZZY:
      return &bb_img_err;
    default:
      return &bb_img_ready;
  }
}

static void refresh_topbar(void) {
  if (s_st.topbar_icon != NULL) {
    lv_image_set_src(s_st.topbar_icon, state_icon(s_st.state));
  }
  if (s_st.topbar_driver_lbl != NULL) {
    lv_label_set_text(s_st.topbar_driver_lbl,
                      s_st.driver_buf[0] != '\0' ? s_st.driver_buf : "---");
  }
  if (s_st.topbar_session_lbl != NULL) {
    lv_label_set_text(s_st.topbar_session_lbl,
                      s_st.session_buf[0] != '\0' ? s_st.session_buf : "--------");
  }
  if (s_st.topbar_bat_container != NULL) {
    bb_power_state_t pwr = {0};
    bb_power_get_state(&pwr);
    if (!pwr.supported || !pwr.available) {
      lv_obj_add_flag(s_st.topbar_bat_container, LV_OBJ_FLAG_HIDDEN);
    } else {
      lv_obj_clear_flag(s_st.topbar_bat_container, LV_OBJ_FLAG_HIDDEN);
      int pct = pwr.percent < 0 ? 0 : (pwr.percent > 100 ? 100 : pwr.percent);
      lv_obj_set_width(s_st.topbar_bat_fill, (pct * 18) / 100);
      lv_obj_set_style_bg_color(s_st.topbar_bat_fill,
                                lv_color_hex(pwr.low ? UI_ERROR_FG : UI_ME_ACCENT), 0);
      char buf[8];
      snprintf(buf, sizeof(buf), "%d", pct);
      lv_label_set_text(s_st.topbar_bat_lbl, buf);
    }
  }
}

static const char* per_driver_face(const char* driver_name) {
  if (driver_name == NULL || driver_name[0] == '\0') return "(^_^)";
  if (strcmp(driver_name, "claude-code") == 0) return "(^_^)";
  if (strcmp(driver_name, "openclaw")    == 0) return "(O_O)";
  if (strcmp(driver_name, "ollama")      == 0) return "(v_v)";
  if (strcmp(driver_name, "opencode")    == 0) return "(._.)";
  return "(^_^)";
}

static void refresh_buddy_text(void) {
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

/* Apply per-state animation. Caller must have already stopped previous anims
 * via stop_all_anims() and refreshed text via refresh_buddy_text().
 *
 * Amplitudes are tuned for the compact topbar buddy area (~86×20px).
 * Vertical bob is ±1px (was ±2-8 in the old 80×154 panel). */
static void apply_state_anim(bb_agent_state_t state) {
  if (s_st.face_lbl == NULL || s_st.mood_lbl == NULL) return;
  switch (state) {
    case BB_AGENT_STATE_SLEEP:
      /* Slow fade 30%↔100%. */
      start_pulse_opa(s_st.face_lbl, LV_OPA_30, LV_OPA_COVER, 1800);
      start_pulse_opa(s_st.mood_lbl, LV_OPA_30, LV_OPA_COVER, 1800);
      break;
    case BB_AGENT_STATE_IDLE:
      /* Gentle ±1px float (reduced from ±2 for topbar). */
      start_bob_y(s_st.face_lbl, FACE_Y0, 1, 1600);
      break;
    case BB_AGENT_STATE_BUSY:
      /* Cycle dots in mood text every 400ms; face stays still. */
      s_st.dots_phase = 0;
      lv_label_set_text(s_st.mood_lbl, "thinking");
      s_st.dots_timer = lv_timer_create(busy_dots_timer_cb, 400, NULL);
      break;
    case BB_AGENT_STATE_SPEAKING:
      /* Left/right sway (reduced from ±4 to ±2 for topbar). */
      start_sway_x(s_st.face_lbl, FACE_X0, 2, 600);
      break;
    case BB_AGENT_STATE_HEART:
      /* Heartbeat: scale 95%↔105% (tighter range for topbar). */
      start_pulse_zoom(s_st.face_lbl, 243, 269, 700);
      break;
    case BB_AGENT_STATE_LISTENING:
      /* Small bob + mood pulse. */
      start_bob_y(s_st.face_lbl, FACE_Y0, 1, 900);
      start_pulse_opa(s_st.mood_lbl, LV_OPA_60, LV_OPA_COVER, 900);
      break;
    case BB_AGENT_STATE_DIZZY:
      /* Horizontal shake (reduced amplitude). */
      start_shake_x(s_st.face_lbl, FACE_X0, 120);
      break;
    case BB_AGENT_STATE_ATTENTION:
      /* Color blink BUDDY_FG↔ATTN_FG. */
      start_color_blink(s_st.face_lbl, 500);
      lv_obj_set_style_text_color(s_st.mood_lbl, lv_color_hex(UI_ATTN_FG), 0);
      break;
    case BB_AGENT_STATE_CELEBRATE:
      /* Bounce + festive color (reduced from ±8 to ±2 for topbar). */
      start_bob_y(s_st.face_lbl, FACE_Y0, 2, 350);
      lv_obj_set_style_text_color(s_st.face_lbl, lv_color_hex(UI_CELEB_FG), 0);
      lv_obj_set_style_text_color(s_st.mood_lbl, lv_color_hex(UI_CELEB_FG), 0);
      break;
  }
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

static void theme_scroll_transcript(int lines) {
  if (s_st.transcript == NULL || lines == 0) return;
  int32_t step = lv_font_get_line_height(theme_font()) * lines;
  lv_obj_scroll_by_bounded(s_st.transcript, 0, step, LV_ANIM_OFF);
}

static lv_obj_t* make_msg_label(uint32_t bg_color, uint32_t fg_color,
                                lv_text_align_t align, int italic) {
  lv_obj_t* lbl = lv_label_create(s_st.transcript);
  lv_label_set_long_mode(lbl, LV_LABEL_LONG_MODE_WRAP);
  lv_obj_set_width(lbl, lv_pct(100));
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
    ESP_LOGW(TAG, "buddy-anim on_enter: already built");
    return;
  }
  if (parent == NULL) return;

  s_st.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_st.root);
  lv_obj_set_size(s_st.root, lv_pct(100), lv_pct(100));
  lv_obj_set_style_bg_color(s_st.root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_st.root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_st.root, LV_OBJ_FLAG_SCROLLABLE);

  /* ── Topbar: [icon] driver  session  [battery]  (^_^) ready ── */
  s_st.topbar = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.topbar);
  lv_obj_set_size(s_st.topbar, 320, TOPBAR_H);
  lv_obj_align(s_st.topbar, LV_ALIGN_TOP_LEFT, 0, SCREEN_CORNER_INSET_Y);
  lv_obj_clear_flag(s_st.topbar, LV_OBJ_FLAG_SCROLLABLE);

  s_st.topbar_icon = lv_image_create(s_st.topbar);
  lv_image_set_src(s_st.topbar_icon, &bb_img_ready);
  lv_obj_set_size(s_st.topbar_icon, 16, 16);
  lv_obj_set_pos(s_st.topbar_icon, SCREEN_CORNER_INSET_X, (TOPBAR_H - 16) / 2);

  s_st.topbar_driver_lbl = lv_label_create(s_st.topbar);
  lv_obj_set_width(s_st.topbar_driver_lbl, 80);
  lv_obj_set_style_text_font(s_st.topbar_driver_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.topbar_driver_lbl, lv_color_hex(UI_STATUS_FG), 0);
  lv_label_set_long_mode(s_st.topbar_driver_lbl, LV_LABEL_LONG_MODE_DOTS);
  lv_obj_set_pos(s_st.topbar_driver_lbl, SCREEN_CORNER_INSET_X + 20, 2);

  s_st.topbar_session_lbl = lv_label_create(s_st.topbar);
  lv_obj_set_width(s_st.topbar_session_lbl, 60);
  lv_obj_set_style_text_font(s_st.topbar_session_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.topbar_session_lbl, lv_color_hex(UI_TEXT_DIM), 0);
  lv_label_set_long_mode(s_st.topbar_session_lbl, LV_LABEL_LONG_MODE_DOTS);
  lv_obj_set_pos(s_st.topbar_session_lbl, SCREEN_CORNER_INSET_X + 104, 2);

  /* Battery widget — positioned left of the buddy area */
  {
    s_st.topbar_bat_container = lv_obj_create(s_st.topbar);
    lv_obj_remove_style_all(s_st.topbar_bat_container);
    lv_obj_set_size(s_st.topbar_bat_container, 44, 14);
    lv_obj_set_pos(s_st.topbar_bat_container,
                   320 - SCREEN_CORNER_INSET_X - BUDDY_TOPBAR_W - 48,
                   (TOPBAR_H - 14) / 2);
    lv_obj_clear_flag(s_st.topbar_bat_container, LV_OBJ_FLAG_SCROLLABLE);

    s_st.topbar_bat_fill = lv_obj_create(s_st.topbar_bat_container);
    lv_obj_remove_style_all(s_st.topbar_bat_fill);
    lv_obj_set_size(s_st.topbar_bat_fill, 18, 8);
    lv_obj_set_pos(s_st.topbar_bat_fill, 2, 3);
    lv_obj_set_style_radius(s_st.topbar_bat_fill, 1, 0);
    lv_obj_set_style_bg_color(s_st.topbar_bat_fill, lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_st.topbar_bat_fill, LV_OPA_COVER, 0);

    s_st.topbar_bat_frame = lv_image_create(s_st.topbar_bat_container);
    lv_image_set_src(s_st.topbar_bat_frame, &bb_el_battery_frame_26x12);
    lv_obj_set_pos(s_st.topbar_bat_frame, 0, 1);

    s_st.topbar_bat_lbl = lv_label_create(s_st.topbar_bat_container);
    lv_obj_set_width(s_st.topbar_bat_lbl, 16);
    lv_obj_set_style_text_color(s_st.topbar_bat_lbl, lv_color_hex(UI_STATUS_FG), 0);
    lv_obj_set_style_text_font(s_st.topbar_bat_lbl, theme_font(), 0);
    lv_obj_set_style_text_align(s_st.topbar_bat_lbl, LV_TEXT_ALIGN_RIGHT, 0);
    lv_label_set_long_mode(s_st.topbar_bat_lbl, LV_LABEL_LONG_MODE_CLIP);
    lv_label_set_text(s_st.topbar_bat_lbl, "--");
    lv_obj_set_pos(s_st.topbar_bat_lbl, 28, 0);
  }

  /* ── Buddy area in topbar (right side): face + mood side by side ── */
  s_st.topbar_buddy = lv_obj_create(s_st.topbar);
  lv_obj_remove_style_all(s_st.topbar_buddy);
  lv_obj_set_size(s_st.topbar_buddy, BUDDY_TOPBAR_W, TOPBAR_H);
  lv_obj_set_pos(s_st.topbar_buddy, 320 - SCREEN_CORNER_INSET_X - BUDDY_TOPBAR_W, 0);
  lv_obj_clear_flag(s_st.topbar_buddy, LV_OBJ_FLAG_SCROLLABLE);

  s_st.face_lbl = lv_label_create(s_st.topbar_buddy);
  lv_obj_set_size(s_st.face_lbl, 54, TOPBAR_H);
  lv_label_set_long_mode(s_st.face_lbl, LV_LABEL_LONG_MODE_CLIP);
  lv_obj_set_pos(s_st.face_lbl, FACE_X0, FACE_Y0);
  lv_obj_set_style_text_font(s_st.face_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.face_lbl, lv_color_hex(UI_BUDDY_FG), 0);
  lv_obj_set_style_text_align(s_st.face_lbl, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_style_transform_pivot_x(s_st.face_lbl, 27, 0);
  lv_obj_set_style_transform_pivot_y(s_st.face_lbl, TOPBAR_H / 2, 0);

  s_st.mood_lbl = lv_label_create(s_st.topbar_buddy);
  lv_obj_set_size(s_st.mood_lbl, 32, TOPBAR_H);
  lv_obj_set_pos(s_st.mood_lbl, MOOD_X0 + 54, MOOD_Y0);
  lv_obj_set_style_text_font(s_st.mood_lbl, theme_font(), 0);
  lv_obj_set_style_text_color(s_st.mood_lbl, lv_color_hex(UI_BUDDY_DIM), 0);
  lv_obj_set_style_text_align(s_st.mood_lbl, LV_TEXT_ALIGN_LEFT, 0);
  lv_label_set_long_mode(s_st.mood_lbl, LV_LABEL_LONG_MODE_DOTS);

  /* ── Transcript (full width, below topbar) ── */
  s_st.transcript = lv_obj_create(s_st.root);
  lv_obj_remove_style_all(s_st.transcript);
  lv_obj_set_size(s_st.transcript, 320, MIDDLE_H);
  lv_obj_align(s_st.transcript, LV_ALIGN_TOP_LEFT, 0,
               SCREEN_CORNER_INSET_Y + TOPBAR_H + TOPBAR_GAP);
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

  s_st.active_assistant = NULL;
  s_st.dots_timer = NULL;
  s_st.dots_phase = 0;
  s_st.built = 1;
  refresh_topbar();
  refresh_buddy_text();
  apply_state_anim(s_st.state);
}

static void theme_on_exit(void) {
  if (!s_st.built) return;
  stop_all_anims();
  if (s_st.root != NULL) {
    lv_obj_del(s_st.root);
  }
  s_st.root = NULL;
  s_st.topbar = NULL;
  s_st.topbar_icon = NULL;
  s_st.topbar_driver_lbl = NULL;
  s_st.topbar_session_lbl = NULL;
  s_st.topbar_bat_container = NULL;
  s_st.topbar_bat_fill = NULL;
  s_st.topbar_bat_frame = NULL;
  s_st.topbar_bat_lbl = NULL;
  s_st.topbar_buddy = NULL;
  s_st.transcript = NULL;
  s_st.face_lbl = NULL;
  s_st.mood_lbl = NULL;
  s_st.active_assistant = NULL;
  s_st.built = 0;
}

static void theme_set_state(bb_agent_state_t state) {
  s_st.state = state;
  if (!s_st.built) return;
  stop_all_anims();
  refresh_topbar();
  refresh_buddy_text();
  apply_state_anim(state);
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
    s_st.active_assistant = make_msg_label(UI_AI_ACCENT, UI_TEXT_MAIN,
                                           LV_TEXT_ALIGN_LEFT, 0);
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
    refresh_buddy_text();
  }
}

static void theme_set_session(const char* sid_short) {
  if (sid_short == NULL) return;
  strncpy(s_st.session_buf, sid_short, sizeof(s_st.session_buf) - 1);
  s_st.session_buf[sizeof(s_st.session_buf) - 1] = '\0';
  if (s_st.built) refresh_topbar();
}

/* Phase S3 — history replay. Same structure as text-only theme: append at
 * end without claiming active_assistant (avoids streaming chunk pollution),
 * prepend at index 0 for "scroll-to-top → load earlier" pagination. */
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

static int theme_is_transcript_at_top(void) {
  if (!s_st.built || s_st.transcript == NULL) return 0;
  return lv_obj_get_scroll_top(s_st.transcript) <= 4 ? 1 : 0;
}

static void theme_scroll_transcript_to_bottom(void) {
  scroll_to_bottom();
}

static const bb_agent_theme_t s_buddy_anim_theme = {
    .name = "buddy-anim",
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

void bb_theme_buddy_anim_init(void) {
  bb_agent_theme_register(&s_buddy_anim_theme);
}
