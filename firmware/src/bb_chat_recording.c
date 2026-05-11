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
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
static const lv_font_t* rec_font(void) { return &lv_font_bbclaw_cjk; }
#else
static const lv_font_t* rec_font(void) { return lv_font_get_default(); }
#endif

/* ── colour palette (matches bb_theme_buddy_anim / bb_lvgl_display) ── */
#define UI_SCR_BG    0x0a0e0c
#define UI_TEXT_MAIN 0xd8ebe4
#define UI_TEXT_DIM  0x7a9a8c
#define UI_ME_ACCENT 0x2ec4a0

/* ── VU meter geometry ── */
#define REC_BAR_COUNT   7
#define REC_BAR_W       10
#define REC_BAR_GAP     4
#define REC_BAR_MIN_H   4
#define REC_BAR_MAX_H   24

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
  lv_obj_t*   bars[REC_BAR_COUNT];
  lv_timer_t* timer;

  /* written from audio task, read from LVGL timer — volatile is sufficient
   * for these single-byte / pointer-sized values on Xtensa. */
  volatile uint8_t  level_pct;
  volatile int      voiced;
  volatile int64_t  updated_ms;

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

static void set_bar_height(lv_obj_t* bar, int h) {
  if (bar == NULL) return;
  if (h < REC_BAR_MIN_H) h = REC_BAR_MIN_H;
  if (h > REC_BAR_MAX_H) h = REC_BAR_MAX_H;
  lv_obj_set_size(bar, REC_BAR_W, h);
  lv_obj_set_y(bar, REC_BAR_MAX_H - h);
}

static void reset_meter_visuals(void) {
  for (int i = 0; i < REC_BAR_COUNT; i++) {
    s_rec.bar_visual[i] = REC_BAR_MIN_H;
    set_bar_height(s_rec.bars[i], REC_BAR_MIN_H);
    if (s_rec.bars[i] != NULL) {
      lv_obj_set_style_bg_opa(s_rec.bars[i], LV_OPA_70, 0);
    }
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
    set_bar_height(s_rec.bars[i], cur);

    if (s_rec.bars[i] != NULL) {
      lv_obj_set_style_bg_opa(s_rec.bars[i],
                              voiced ? LV_OPA_COVER : LV_OPA_70, 0);
    }
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

void bb_chat_recording_create(lv_obj_t* parent, int width, int height_px) {
  if (s_rec.built) return;
  if (parent == NULL) return;

  const lv_font_t* font = rec_font();
  const int w = width;
  const int h = height_px;

  /* Semi-transparent overlay container — hidden by default */
  s_rec.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_rec.root);
  lv_obj_set_size(s_rec.root, w, h);
  lv_obj_set_pos(s_rec.root, 0, 0);
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

  /* ── VU meter (7 bars, centered) ── */
  const int meter_w = REC_BAR_COUNT * REC_BAR_W + (REC_BAR_COUNT - 1) * REC_BAR_GAP;
  const int meter_x = (w - meter_w) / 2;
  /* Position meter below title, leaving room for hint at bottom */
  const int meter_y = h - REC_BAR_MAX_H - 20;

  s_rec.meter = lv_obj_create(s_rec.root);
  lv_obj_remove_style_all(s_rec.meter);
  lv_obj_set_size(s_rec.meter, meter_w, REC_BAR_MAX_H);
  lv_obj_set_pos(s_rec.meter, meter_x, meter_y);
  lv_obj_clear_flag(s_rec.meter, LV_OBJ_FLAG_SCROLLABLE);

  for (int i = 0; i < REC_BAR_COUNT; i++) {
    s_rec.bars[i] = lv_obj_create(s_rec.meter);
    lv_obj_remove_style_all(s_rec.bars[i]);
    lv_obj_set_pos(s_rec.bars[i], i * (REC_BAR_W + REC_BAR_GAP),
                   REC_BAR_MAX_H - REC_BAR_MIN_H);
    lv_obj_set_style_radius(s_rec.bars[i], 3, 0);
    lv_obj_set_style_bg_color(s_rec.bars[i], lv_color_hex(UI_ME_ACCENT), 0);
    lv_obj_set_style_bg_opa(s_rec.bars[i], LV_OPA_70, 0);
    set_bar_height(s_rec.bars[i], REC_BAR_MIN_H);
    s_rec.bar_visual[i] = REC_BAR_MIN_H;
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

  s_rec.built = 1;
}

void bb_chat_recording_show(void) {
  if (!s_rec.built || s_rec.root == NULL) return;
  reset_meter_visuals();
  s_rec.anim_tick = 0;
  lv_obj_clear_flag(s_rec.root, LV_OBJ_FLAG_HIDDEN);
  lv_obj_move_foreground(s_rec.root);
}

void bb_chat_recording_hide(void) {
  if (!s_rec.built || s_rec.root == NULL) return;
  lv_obj_add_flag(s_rec.root, LV_OBJ_FLAG_HIDDEN);
  /* Reset level so stale data doesn't flash on next show */
  s_rec.level_pct  = 0;
  s_rec.voiced     = 0;
  s_rec.updated_ms = 0;
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

