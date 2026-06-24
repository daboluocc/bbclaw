/**
 * Blocking-prompt confirm page. See bb_page_prompt_select.h.
 *
 * Layout (320×172 display):
 *   Y=  8  question (cool white, wrapped, up to ~3 lines)
 *   Y= 64  option rows (one per menu option; selected = accent + "▸")
 *   Y=138  dot countdown bar (auto-deny)
 *   Y=156  "UP/DN move  OK ok  BACK no" hint (dim)
 *
 * Timer design mirrors bb_page_ota_confirm: the 1 Hz LVGL timer only repaints the
 * dot bar and sets s_timed_out — the radio-app main loop drains that and calls
 * handle_nav(0) (deny) so the user callback runs outside the LVGL lock.
 */
#include "bb_page_prompt_select.h"

#include <stdio.h>
#include <string.h>

#include "bb_config.h"
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
#define CDOWN_CELLS      30
#define CDOWN_CELL_DOT   5
#define CDOWN_CELL_PITCH 8
#define CDOWN_BAR_W      ((CDOWN_CELLS - 1) * CDOWN_CELL_PITCH + CDOWN_CELL_DOT)

/* ── Y positions ── */
#define Q_Y     8
#define OPT_Y0  64
#define OPT_PITCH 22
#define BAR_Y   138
#define HINT_Y  156

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
  lv_label_set_long_mode(q, LV_LABEL_LONG_WRAP);
  lv_obj_set_width(q, BBCLAW_ST7789_WIDTH - 16);
  lv_obj_set_pos(q, 8, Q_Y);
  lv_label_set_text(q, s_prompt.question[0] ? s_prompt.question : "Confirm?");

  /* Option rows */
  for (int i = 0; i < s_prompt.n_options; i++) {
    lv_obj_t *o = lv_label_create(s_root);
    lv_obj_set_style_text_font(o, small_font(), 0);
    lv_obj_set_style_text_align(o, LV_TEXT_ALIGN_LEFT, 0);
    lv_label_set_long_mode(o, LV_LABEL_LONG_DOT);
    lv_obj_set_width(o, BBCLAW_ST7789_WIDTH - 24);
    lv_obj_set_pos(o, 16, OPT_Y0 + i * OPT_PITCH);
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
    lv_obj_set_style_radius(c, 1, 0);
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
