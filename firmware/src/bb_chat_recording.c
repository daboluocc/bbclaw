/**
 * CHAT recording overlay — semi-transparent mic/VU mask shown during PTT.
 *
 * Layout (within the transcript area, typically y=32 h=112 on the 1.47" panel):
 *   - Semi-transparent background (LV_OPA_60 of UI_SCR_BG)
 *   - Mic badge (circular, centered, top third)
 *   - "正在聆听" title label
 *   - VU meter bars (7 bars, centered)
 *   - "松开发送" hint label (bottom)
 *
 * Thread safety: bb_chat_recording_set_level() may be called from the audio
 * task. It only writes to volatile scalars; the LVGL timer callback reads them
 * from the LVGL task, so no mutex is needed for these small atomic writes.
 */
#include "bb_chat_recording.h"

#include <string.h>

#include "bb_lvgl_assets.h"
#include "bb_time.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
static const lv_font_t* rec_font(void) { return &lv_font_bbclaw_cjk; }
#else
static const lv_font_t* rec_font(void) { return lv_font_get_default(); }
#endif

/* ── colour palette — design/UI_DESIGN_LANGUAGE.md tokens ── */
#define UI_SCR_BG    BB_UI_BG
#define UI_TEXT_MAIN BB_UI_DOT_LIT
#define UI_TEXT_DIM  BB_UI_TEXT_DIM
#define UI_DOT_GHOST BB_UI_DOT_GHOST
#define UI_ME_ACCENT BB_UI_ACCENT

/* ── VU meter geometry — dot-matrix columns (design/UI_DESIGN_LANGUAGE.md):
 * 7 columns × 5 rows of small dots, lit bottom-up with level; the peak dot
 * flashes teal while voiced. Smoothing still runs in "virtual px" units
 * (REC_BAR_MIN_H..REC_BAR_MAX_H) and maps to lit-row count at paint time. */
#define REC_BAR_COUNT   7
#define REC_DOT_ROWS    5
#define REC_DOT         4  /* dot diameter — compact grid (base is 5/9)     */
#define REC_DOT_PITCH   7
#define REC_BAR_MIN_H   4
#define REC_BAR_MAX_H   24
#define REC_METER_W     ((REC_BAR_COUNT - 1) * REC_DOT_PITCH + REC_DOT)
#define REC_METER_H     ((REC_DOT_ROWS - 1) * REC_DOT_PITCH + REC_DOT)

/* ── timing ── */
#define REC_UPDATE_MS       48   /* timer period — matches bb_lvgl_display */
#define REC_LEVEL_STALE_MS  280  /* zero-out level if no update within this */

/* ── module state ── */
typedef struct {
  lv_obj_t*   root;
  lv_obj_t*   badge_cont;
  lv_obj_t*   img_badge;
  lv_obj_t*   lbl_title;
  lv_obj_t*   lbl_hint;
  lv_obj_t*   meter;
  lv_obj_t*   dots[REC_BAR_COUNT][REC_DOT_ROWS]; /* [col][row], row 0 = top */
  lv_timer_t* timer;

  /* written from audio task, read from LVGL timer — volatile is sufficient
   * for these single-byte / pointer-sized values on Xtensa. */
  volatile uint8_t  level_pct;
  volatile int      voiced;
  volatile int64_t  updated_ms;

  /* Lock-free visibility request. Written by any task (no lock needed),
   * applied by rec_timer_cb under the LVGL lock it already holds. This
   * keeps show/hide off the LVGL lock path where 50–500 ms lock races
   * were dropping visibility transitions.
   *   -1 = no pending request
   *    0 = caller wants overlay hidden
   *    1 = caller wants overlay shown (resets anim state)
   */
  volatile int      want_visible;

  uint32_t anim_tick;
  uint8_t  bar_visual[REC_BAR_COUNT];
  int      built;
} bb_rec_state_t;

static bb_rec_state_t s_rec = {0};

/* ── helpers ── */

static const lv_image_dsc_t* rec_anim_icon(uint32_t tick) {
  switch (tick % 3U) {
    case 0:  return &bb_img_rec_1;
    case 1:  return &bb_img_rec_2;
    default: return &bb_img_rec_3;
  }
}

/* Map the smoothed virtual height (REC_BAR_MIN_H..REC_BAR_MAX_H) of one
 * column to lit dots, bottom-up. Lit dots are cool white; the peak dot is
 * teal while voiced (the boot/netconn "newest element flashes teal" beat). */
static void set_column(int col, int h, int voiced) {
  if (h < REC_BAR_MIN_H) h = REC_BAR_MIN_H;
  if (h > REC_BAR_MAX_H) h = REC_BAR_MAX_H;
  int rows_lit = 1 + ((h - REC_BAR_MIN_H) * (REC_DOT_ROWS - 1) +
                      (REC_BAR_MAX_H - REC_BAR_MIN_H) / 2) /
                         (REC_BAR_MAX_H - REC_BAR_MIN_H);
  for (int r = 0; r < REC_DOT_ROWS; r++) {
    lv_obj_t* d = s_rec.dots[col][r];
    if (d == NULL) continue;
    int from_bottom = REC_DOT_ROWS - r; /* 1 = bottom row, ROWS = top row */
    uint32_t color;
    if (from_bottom > rows_lit) {
      color = UI_DOT_GHOST;
    } else if (from_bottom == rows_lit && voiced) {
      color = UI_ME_ACCENT; /* peak dot */
    } else {
      color = UI_TEXT_MAIN;
    }
    lv_obj_set_style_bg_color(d, lv_color_hex(color), 0);
  }
}

static void reset_meter_visuals(void) {
  for (int i = 0; i < REC_BAR_COUNT; i++) {
    s_rec.bar_visual[i] = REC_BAR_MIN_H;
    set_column(i, REC_BAR_MIN_H, 0);
  }
  if (s_rec.img_badge != NULL) {
    lv_image_set_src(s_rec.img_badge, &bb_img_tx);
  }
  if (s_rec.badge_cont != NULL) {
    lv_obj_set_style_bg_color(s_rec.badge_cont, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_bg_opa(s_rec.badge_cont, LV_OPA_20, 0);
    lv_obj_set_style_border_color(s_rec.badge_cont, lv_color_hex(UI_TEXT_DIM), 0);
    lv_obj_set_style_border_opa(s_rec.badge_cont, LV_OPA_40, 0);
  }
}

/* ── timer callback — runs in LVGL task every REC_UPDATE_MS ── */

static void rec_timer_cb(lv_timer_t* t) {
  (void)t;
  if (!s_rec.built || s_rec.root == NULL) return;

  /* Apply any pending show/hide request before animating. This is the only
   * place that mutates HIDDEN, so the caller-visible bb_chat_recording_show /
   * bb_chat_recording_hide stay lock-free. */
  int want = s_rec.want_visible;
  if (want == 1) {
    s_rec.want_visible = -1;
    reset_meter_visuals();
    s_rec.anim_tick = 0;
    lv_obj_clear_flag(s_rec.root, LV_OBJ_FLAG_HIDDEN);
    lv_obj_move_foreground(s_rec.root);
  } else if (want == 0) {
    s_rec.want_visible = -1;
    lv_obj_add_flag(s_rec.root, LV_OBJ_FLAG_HIDDEN);
    s_rec.level_pct  = 0;
    s_rec.voiced     = 0;
    s_rec.updated_ms = 0;
  }

  if (lv_obj_has_flag(s_rec.root, LV_OBJ_FLAG_HIDDEN)) return;

  /* Bell-shaped amplitude profile across 7 bars */
  static const uint8_t kProfiles[REC_BAR_COUNT] = {40, 60, 80, 100, 80, 60, 40};

  uint8_t level_pct = s_rec.level_pct;
  int     voiced    = s_rec.voiced;
  int64_t updated   = s_rec.updated_ms;

  const int64_t now_ms = bb_now_ms();
  if (updated == 0 || (now_ms - updated) > REC_LEVEL_STALE_MS) {
    level_pct = 0;
    voiced    = 0;
  }

  s_rec.anim_tick++;

  for (int i = 0; i < REC_BAR_COUNT; i++) {
    int wobble = 0;
    if (level_pct > 3U) {
      wobble = (int)((s_rec.anim_tick + (uint32_t)(i * 3)) % 7U) - 3;
    }
    int target_h = REC_BAR_MIN_H +
                   (int)((level_pct * (uint32_t)kProfiles[i] *
                          (REC_BAR_MAX_H - REC_BAR_MIN_H)) / 10000U);
    target_h += wobble;
    if (target_h < REC_BAR_MIN_H) target_h = REC_BAR_MIN_H;
    if (target_h > REC_BAR_MAX_H) target_h = REC_BAR_MAX_H;

    /* Smooth: attack fast, decay slower */
    int cur = (int)s_rec.bar_visual[i];
    if (target_h > cur)      cur += (target_h - cur + 1) / 2;
    else if (target_h < cur) cur -= (cur - target_h + 2) / 3;
    s_rec.bar_visual[i] = (uint8_t)cur;
    set_column(i, cur, voiced);
  }

  /* Mic badge: animate icon when voiced, static tx icon otherwise */
  if (s_rec.badge_cont != NULL) {
    lv_obj_set_style_bg_color(s_rec.badge_cont,
                              lv_color_hex(voiced ? UI_ME_ACCENT : UI_TEXT_DIM), 0);
    lv_obj_set_style_bg_opa(s_rec.badge_cont,
                            voiced ? (lv_opa_t)(48 + (level_pct * 52U) / 100U)
                                   : LV_OPA_20, 0);
    lv_obj_set_style_border_color(s_rec.badge_cont,
                                  lv_color_hex(voiced ? UI_ME_ACCENT : UI_TEXT_DIM), 0);
    lv_obj_set_style_border_opa(s_rec.badge_cont,
                                voiced ? LV_OPA_COVER : LV_OPA_40, 0);
  }
  if (s_rec.img_badge != NULL) {
    lv_image_set_src(s_rec.img_badge,
                     voiced ? rec_anim_icon(s_rec.anim_tick) : &bb_img_tx);
  }
}

/* ── public API ── */

void bb_chat_recording_create(lv_obj_t* parent, int width, int height_px,
                              int y_offset) {
  if (s_rec.built) return;
  if (parent == NULL) return;

  const lv_font_t* font = rec_font();
  const int w = width;
  const int h = height_px;

  /* Semi-transparent overlay container — hidden by default */
  s_rec.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_rec.root);
  lv_obj_set_size(s_rec.root, w, h);
  lv_obj_set_pos(s_rec.root, 0, y_offset);
  lv_obj_set_style_bg_color(s_rec.root, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_rec.root, LV_OPA_80, 0);
  lv_obj_clear_flag(s_rec.root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_add_flag(s_rec.root, LV_OBJ_FLAG_HIDDEN);

  /* ── Mic badge (circular, centered, top area) ── */
  const int badge_size = 36;
  const int badge_x    = (w - badge_size) / 2;
  const int badge_y    = 8;

  s_rec.badge_cont = lv_obj_create(s_rec.root);
  lv_obj_remove_style_all(s_rec.badge_cont);
  lv_obj_set_size(s_rec.badge_cont, badge_size, badge_size);
  lv_obj_set_pos(s_rec.badge_cont, badge_x, badge_y);
  lv_obj_set_style_radius(s_rec.badge_cont, LV_RADIUS_CIRCLE, 0);
  lv_obj_set_style_border_width(s_rec.badge_cont, 1, 0);
  lv_obj_set_style_border_color(s_rec.badge_cont, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_bg_color(s_rec.badge_cont, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_bg_opa(s_rec.badge_cont, LV_OPA_20, 0);

  s_rec.img_badge = lv_image_create(s_rec.badge_cont);
  lv_image_set_src(s_rec.img_badge, &bb_img_tx);
  lv_obj_center(s_rec.img_badge);

  /* ── "正在聆听" title ── */
  s_rec.lbl_title = lv_label_create(s_rec.root);
  lv_obj_set_width(s_rec.lbl_title, w);
  lv_obj_set_style_text_color(s_rec.lbl_title, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_rec.lbl_title, font, 0);
  lv_obj_set_style_text_align(s_rec.lbl_title, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_rec.lbl_title, "正在聆听");
  lv_obj_set_pos(s_rec.lbl_title, 0, badge_y + badge_size + 6);

  /* ── VU meter — 7×5 dot-matrix columns, centered ── */
  const int meter_x = (w - REC_METER_W) / 2;
  /* Position meter below title, leaving room for hint at bottom */
  const int meter_y = h - REC_METER_H - 18;

  s_rec.meter = lv_obj_create(s_rec.root);
  lv_obj_remove_style_all(s_rec.meter);
  lv_obj_set_size(s_rec.meter, REC_METER_W, REC_METER_H);
  lv_obj_set_pos(s_rec.meter, meter_x, meter_y);
  lv_obj_clear_flag(s_rec.meter, LV_OBJ_FLAG_SCROLLABLE);

  for (int i = 0; i < REC_BAR_COUNT; i++) {
    for (int r = 0; r < REC_DOT_ROWS; r++) {
      lv_obj_t* d = lv_obj_create(s_rec.meter);
      lv_obj_remove_style_all(d);
      lv_obj_set_size(d, REC_DOT, REC_DOT);
      lv_obj_set_pos(d, i * REC_DOT_PITCH, r * REC_DOT_PITCH);
      lv_obj_set_style_radius(d, LV_RADIUS_CIRCLE, 0);
      lv_obj_set_style_bg_color(d, lv_color_hex(UI_DOT_GHOST), 0);
      lv_obj_set_style_bg_opa(d, LV_OPA_COVER, 0);
      lv_obj_clear_flag(d, LV_OBJ_FLAG_SCROLLABLE);
      s_rec.dots[i][r] = d;
    }
    s_rec.bar_visual[i] = REC_BAR_MIN_H;
    set_column(i, REC_BAR_MIN_H, 0);
  }

  /* ── "松开发送" hint (bottom) ── */
  s_rec.lbl_hint = lv_label_create(s_rec.root);
  lv_obj_set_width(s_rec.lbl_hint, w);
  lv_obj_set_style_text_color(s_rec.lbl_hint, lv_color_hex(UI_TEXT_DIM), 0);
  lv_obj_set_style_text_font(s_rec.lbl_hint, font, 0);
  lv_obj_set_style_text_align(s_rec.lbl_hint, LV_TEXT_ALIGN_CENTER, 0);
  lv_label_set_text(s_rec.lbl_hint, "松开发送");
  lv_obj_set_pos(s_rec.lbl_hint, 0, h - 16);

  /* ── Update timer ── */
  s_rec.timer = lv_timer_create(rec_timer_cb, REC_UPDATE_MS, NULL);

  s_rec.want_visible = -1;
  s_rec.built = 1;
}

void bb_chat_recording_show(void) {
  if (!s_rec.built || s_rec.root == NULL) return;
  /* Lock-free: rec_timer_cb picks this up on its next 48 ms tick under the
   * LVGL lock it already holds. Previously callers had to acquire the LVGL
   * lock themselves, and short (50–500 ms) lock timeouts during heavy
   * rendering bursts left the mask in the wrong state. */
  s_rec.want_visible = 1;
}

void bb_chat_recording_hide(void) {
  if (!s_rec.built || s_rec.root == NULL) return;
  s_rec.want_visible = 0;
}

void bb_chat_recording_set_level(uint8_t level_pct, int voiced) {
  /* Called from audio task — only write volatile scalars, no LVGL calls */
  if (level_pct > 100U) level_pct = 100U;
  s_rec.level_pct  = level_pct;
  s_rec.voiced     = voiced ? 1 : 0;
  s_rec.updated_ms = bb_now_ms();
}

void bb_chat_recording_destroy(void) {
  if (!s_rec.built) return;
  /* Delete the timer explicitly — it is not an LVGL object and won't be
   * cleaned up by the parent's lv_obj_del cascade. The LVGL objects
   * (root, badge, bars, labels) are children of the theme root and will
   * be freed when the theme calls lv_obj_del on its own root. Do NOT call
   * lv_obj_del(s_rec.root) here — that would double-free. */
  if (s_rec.timer != NULL) {
    lv_timer_del(s_rec.timer);
    s_rec.timer = NULL;
  }
  memset(&s_rec, 0, sizeof(s_rec));
}

