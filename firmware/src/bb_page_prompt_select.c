/**
 * Blocking-prompt confirm page. See bb_page_prompt_select.h.
 *
 * Layout (320×172 display):
 *   Y=  8  question (cool white, wrapped, up to ~3 lines)
 *   Y= 64  option rows (one per menu option; selected = accent + "▸")
 *   Y=138  dot countdown bar (auto-deny)
 *   Y=156  "UP/DN move  OK ok  BACK no" hint (dim)
 *
 * 竖屏手表（410×502, BB_UI_PORTRAIT）：内容组垂直居中（居中构图天然避开
 * R60 物理圆角），Y 栈按 BB_PROMPT_MAX_OPTIONS=4 预留——修掉方屏遗留的
 * “4 选项与倒计时条重叠”问题；选项行距 52px（≥48px 未来触摸目标），
 * 倒计时条 30 cells 语义不变、几何放大 ~1.6x（同 bb_page_ota_confirm）。
 *
 * Timer design mirrors bb_page_ota_confirm: the 1 Hz LVGL timer only repaints the
 * dot bar and sets s_timed_out — the radio-app main loop drains that and calls
 * handle_nav(0) (deny) so the user callback runs outside the LVGL lock.
 */
#include "bb_page_prompt_select.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
#include "bb_ui_layout.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#if defined(BBCLAW_SIMULATOR)
#define ESP_LOGI(tag, fmt, ...) ((void)(tag))
#define ESP_LOGW(tag, fmt, ...) ((void)(tag))
static int lvgl_port_lock(int t) { (void)t; return 1; }
static void lvgl_port_unlock(void) {}
#else
#include "esp_log.h"
#include "esp_lvgl_port.h"
#endif

static const char *TAG = "bb_page_prompt_select";

/* ── palette ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_ACCENT    BB_UI_ACCENT
#define UI_DOT_LIT   BB_UI_DOT_LIT
#define UI_TEXT_DIM  BB_UI_TEXT_DIM

/* ── countdown bar geometry (same cells as ota_confirm) ── */
#define CDOWN_CELLS      30  /* 30 cells = 30 s，一秒灭一格（倒计时语义勿动） */
#if BB_UI_PORTRAIT
#if BB_DISP_W <= 160
/* 窄竖屏（M5StickS3 135px）：30 格 × pitch12 = 356px 严重溢出屏外，缩到 pitch4/dot3
 * → 条宽 29*4+3 = 119px 居中塞进 135。 */
#define CDOWN_CELL_DOT    3
#define CDOWN_CELL_PITCH  4
#define CDOWN_CELL_RADIUS 1
#else
/* 竖屏手表：dot/pitch 放大 ~1.6x，cell 数不变只放大几何（同 ota_confirm） */
#define CDOWN_CELL_DOT    8
#define CDOWN_CELL_PITCH  12
#define CDOWN_CELL_RADIUS 2
#endif
#else
#define CDOWN_CELL_DOT    5
#define CDOWN_CELL_PITCH  8
#define CDOWN_CELL_RADIUS 1
#endif
#define CDOWN_BAR_W      ((CDOWN_CELLS - 1) * CDOWN_CELL_PITCH + CDOWN_CELL_DOT)

/* ── geometry / Y positions ── */
#if BB_UI_PORTRAIT
#if BB_DISP_W <= 160
/* 窄竖屏（M5StickS3 135px）：手表的 Q_X36/OPT_PITCH52 会让内容组高过 240 被顶出屏外，
 * 且长选项在窄列里换行撞行重叠。改：满宽（边距6）、顶对齐（不居中）、行距 40px 容 ~2 行
 * montserrat14。选项过长仍会紧，但不再重叠。 */
#define Q_X         6
#define Q_W         (BB_DISP_W - 2 * Q_X)
#define Q_H         44  /* ~2-3 行问题文本 */
#define OPT_X       6
#define OPT_W       (BB_DISP_W - 2 * OPT_X)
#define OPT_PITCH   40
#define OPT_TEXT_DY 4
#define Q_Y         6   /* 顶对齐：窄屏若居中会溢出 */
#define OPT_Y0      (Q_Y + Q_H + 6)
#define BAR_Y       (OPT_Y0 + BB_PROMPT_MAX_OPTIONS * OPT_PITCH + 6)
#define HINT_Y      (BAR_Y + CDOWN_CELL_DOT + 10)
#else
/* 内容组整体垂直居中；Y 栈按最大选项数（4）预留，保证 4 行 + 倒计时条 +
 * hint 三者永不重叠（方屏分支该 bug 原样保留，勿在此回移）。 */
#define Q_X         36
#define Q_W         (BB_DISP_W - 2 * Q_X)
#define Q_H         76  /* ~4 行 montserrat14；超出打点省略，不越入选项区 */
#define OPT_X       28
#define OPT_W       (BB_DISP_W - 2 * OPT_X)
#define OPT_PITCH   52  /* 行距 ≥48px：未来触摸目标（ADR-040 §UI） */
#define OPT_TEXT_DY 17  /* 文本在 52px 行内垂直居中 */
#define CONTENT_H   (Q_H + 8 + BB_PROMPT_MAX_OPTIONS * OPT_PITCH + 8 + CDOWN_CELL_DOT + 24 + 18)
#define Q_Y         ((BB_DISP_H - CONTENT_H) / 2)
#define OPT_Y0      (Q_Y + Q_H + 8)
#define BAR_Y       (OPT_Y0 + BB_PROMPT_MAX_OPTIONS * OPT_PITCH + 8)
#define HINT_Y      (BAR_Y + CDOWN_CELL_DOT + 24)
#endif
#else
#define Q_X         8
#define Q_W         (BBCLAW_ST7789_WIDTH - 16)
#define OPT_X       16
#define OPT_W       (BBCLAW_ST7789_WIDTH - 24)
#define OPT_TEXT_DY 0
#define Q_Y     8
#define OPT_Y0  64
#define OPT_PITCH 22
#define BAR_Y   138
#define HINT_Y  156
#endif

#define TIMEOUT_SEC 30 /* under the adapter's PromptTimeout (90s): device denies first */

#if LV_FONT_MONTSERRAT_14
LV_FONT_DECLARE(lv_font_montserrat_14)
#endif

static const lv_font_t *small_font(void) {
#if LV_FONT_MONTSERRAT_14
  return &lv_font_montserrat_14;
#else
  return lv_font_get_default();
#endif
}

/* ── module state ── */
static lv_obj_t                  *s_root;
static lv_obj_t                  *s_opt_lbls[BB_PROMPT_MAX_OPTIONS];
static lv_obj_t                  *s_cells[CDOWN_CELLS];
static lv_timer_t                *s_timer;
static volatile int               s_remaining;
static volatile int               s_timed_out;
static bb_prompt_t                s_prompt; /* a COPY: the event's struct is transient */
static int                        s_sel;    /* highlighted option index */
static bb_page_prompt_select_cb_t s_cb;

/* ── internal helpers (called with LVGL lock held) ── */

static void destroy_now(void) {
  if (s_timer) { lv_timer_del(s_timer); s_timer = NULL; }
  if (s_root)  { lv_obj_del(s_root);  s_root  = NULL; }
  for (int i = 0; i < BB_PROMPT_MAX_OPTIONS; i++) s_opt_lbls[i] = NULL;
}

static void paint_options(void) {
  for (int i = 0; i < s_prompt.n_options && i < BB_PROMPT_MAX_OPTIONS; i++) {
    if (!s_opt_lbls[i]) continue;
    uint32_t col = (i == s_sel) ? UI_ACCENT : UI_TEXT_DIM;
    lv_obj_set_style_text_color(s_opt_lbls[i], lv_color_hex(col), 0);
    char buf[80];
    /* ASCII marker only: the Montserrat-14 glyph set has no ▸, so use ">". */
    snprintf(buf, sizeof(buf), "%s %s. %s", (i == s_sel) ? ">" : " ",
             s_prompt.options[i].key, s_prompt.options[i].label);
    lv_label_set_text(s_opt_lbls[i], buf);
  }
}

static void paint_countdown(int remaining) {
  for (int i = 0; i < CDOWN_CELLS; i++) {
    uint32_t col = (i < remaining) ? UI_ACCENT : UI_DOT_GHOST;
    lv_obj_set_style_bg_color(s_cells[i], lv_color_hex(col), 0);
  }
}

static void prompt_timer_cb(lv_timer_t *t) {
  (void)t;
  if (!s_root) return;
  int rem = s_remaining - 1;
  if (rem < 0) rem = 0;
  s_remaining = rem;
  paint_countdown(rem);
  if (rem == 0) s_timed_out = 1;
}

/* ── public API ── */

void bb_page_prompt_select_show(const bb_prompt_t *prompt, bb_page_prompt_select_cb_t cb) {
  if (prompt == NULL || prompt->n_options <= 0) return;
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "show: lvgl lock timeout");
    return;
  }
  if (s_root) { lvgl_port_unlock(); return; }

  s_prompt = *prompt; /* copy: the caller's struct is on the bbwire2 stack */
  if (s_prompt.n_options > BB_PROMPT_MAX_OPTIONS) s_prompt.n_options = BB_PROMPT_MAX_OPTIONS;
  s_cb        = cb;
  s_remaining = TIMEOUT_SEC;
  s_timed_out = 0;
  /* Safe default: land the selection on the DENY option (claude's last row), so
   * an accidental OK denies rather than approves (ADR-033 §11). */
  s_sel = s_prompt.n_options - 1;

  s_root = lv_obj_create(lv_layer_top());
  lv_obj_remove_style_all(s_root);
  lv_obj_set_size(s_root, BBCLAW_ST7789_WIDTH, BBCLAW_ST7789_HEIGHT);
  lv_obj_set_pos(s_root, 0, 0);
  lv_obj_set_style_bg_color(s_root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_root, LV_OBJ_FLAG_SCROLLABLE | LV_OBJ_FLAG_CLICKABLE);

  /* Question (wrapped) */
  lv_obj_t *q = lv_label_create(s_root);
  lv_obj_set_style_text_color(q, lv_color_hex(UI_DOT_LIT), 0);
  lv_obj_set_style_text_font(q, small_font(), 0);
  lv_obj_set_style_text_align(q, LV_TEXT_ALIGN_CENTER, 0);
#if BB_UI_PORTRAIT
  /* 固定问题区高度 + 打点省略：问题再长也不越入选项区 */
  lv_label_set_long_mode(q, LV_LABEL_LONG_DOT);
  lv_obj_set_size(q, Q_W, Q_H);
#else
  lv_label_set_long_mode(q, LV_LABEL_LONG_WRAP);
  lv_obj_set_width(q, Q_W);
#endif
  lv_obj_set_pos(q, Q_X, Q_Y);
  lv_label_set_text(q, s_prompt.question[0] ? s_prompt.question : "Confirm?");

  /* Option rows */
  for (int i = 0; i < s_prompt.n_options; i++) {
    lv_obj_t *o = lv_label_create(s_root);
    lv_obj_set_style_text_font(o, small_font(), 0);
    lv_obj_set_style_text_align(o, LV_TEXT_ALIGN_LEFT, 0);
    lv_label_set_long_mode(o, LV_LABEL_LONG_DOT);
    lv_obj_set_width(o, OPT_W);
    /* 限高（行距内）让 LONG_DOT 真正截断——否则标签自动长高、超长选项换行撞下一行
     * （窄屏 135px 上尤其明显）。正常 1-2 行选项不受影响。 */
    lv_obj_set_height(o, OPT_PITCH - 4);
    lv_obj_set_pos(o, OPT_X, OPT_Y0 + i * OPT_PITCH + OPT_TEXT_DY);
    s_opt_lbls[i] = o;
  }
  paint_options();

  /* Countdown dot bar */
  int x0 = (BBCLAW_ST7789_WIDTH - CDOWN_BAR_W) / 2;
  for (int i = 0; i < CDOWN_CELLS; i++) {
    lv_obj_t *c = lv_obj_create(s_root);
    lv_obj_remove_style_all(c);
    lv_obj_set_size(c, CDOWN_CELL_DOT, CDOWN_CELL_DOT);
    lv_obj_set_pos(c, x0 + i * CDOWN_CELL_PITCH, BAR_Y);
    lv_obj_set_style_radius(c, CDOWN_CELL_RADIUS, 0);
    lv_obj_set_style_bg_color(c, lv_color_hex(UI_ACCENT), 0);
    lv_obj_set_style_bg_opa(c, LV_OPA_COVER, 0);
    lv_obj_clear_flag(c, LV_OBJ_FLAG_SCROLLABLE);
    s_cells[i] = c;
  }

  /* Hint */
  lv_obj_t *hint = lv_label_create(s_root);
  lv_obj_set_style_text_color(hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(hint, small_font(), 0);
  lv_obj_set_style_text_align(hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(hint, BBCLAW_ST7789_WIDTH);
  lv_obj_set_pos(hint, 0, HINT_Y);
  lv_label_set_text(hint, "UP/DN move  OK ok  BACK no");

  s_timer = lv_timer_create(prompt_timer_cb, 1000, NULL);

  lvgl_port_unlock();
  ESP_LOGI(TAG, "prompt shown id=%s opts=%d", s_prompt.prompt_id, s_prompt.n_options);
}

void bb_page_prompt_select_dismiss(void) {
  if (!lvgl_port_lock(1000)) {
    ESP_LOGW(TAG, "dismiss: lvgl lock timeout");
    return;
  }
  s_cb = NULL;
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "prompt dismissed");
}

int bb_page_prompt_select_active(void) { return s_root ? 1 : 0; }

int bb_page_prompt_select_active_id_is(const char *prompt_id) {
  return s_root && prompt_id && strcmp(s_prompt.prompt_id, prompt_id) == 0;
}

int bb_page_prompt_select_timed_out(void) { return s_timed_out; }

void bb_page_prompt_select_nav_move(int delta) {
  if (!s_root || delta == 0) return;
  if (!lvgl_port_lock(600)) return;
  int n = s_prompt.n_options;
  int sel = s_sel + (delta < 0 ? -1 : 1);
  if (sel < 0) sel = 0;
  if (sel > n - 1) sel = n - 1;
  s_sel = sel;
  paint_options();
  lvgl_port_unlock();
}

void bb_page_prompt_select_handle_nav(int nav_ok) {
  if (!s_root) return;
  if (!lvgl_port_lock(600)) {
    ESP_LOGW(TAG, "handle_nav: lvgl lock timeout");
    return;
  }
  /* OK → the highlighted option; deny → claude's last option (conventionally "No"). */
  int idx = nav_ok ? s_sel : (s_prompt.n_options - 1);
  if (idx < 0) idx = 0;
  if (idx > s_prompt.n_options - 1) idx = s_prompt.n_options - 1;
  char id[sizeof(s_prompt.prompt_id)];
  char key[sizeof(s_prompt.options[0].key)];
  snprintf(id, sizeof(id), "%s", s_prompt.prompt_id);
  snprintf(key, sizeof(key), "%s", s_prompt.options[idx].key);
  bb_page_prompt_select_cb_t cb = s_cb;
  s_cb = NULL;
  s_timed_out = 0;
  destroy_now();
  lvgl_port_unlock();
  ESP_LOGI(TAG, "prompt decision id=%s ok=%d key=%s", id, nav_ok, key);
  if (cb) cb(id, key);
}
