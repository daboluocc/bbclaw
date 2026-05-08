/**
 * STANDBY page — independent full-screen view.
 * Minimal layout: "BBClaw" brand + large clock. No top/bottom bar.
 */
#include "bb_page_standby.h"

#include "bb_config.h"
#include "lvgl.h"

#ifdef BBCLAW_HAVE_CJK_FONT
extern const lv_font_t lv_font_bbclaw_cjk;
#endif

#if defined(CONFIG_LV_FONT_MONTSERRAT_40) || defined(LV_FONT_MONTSERRAT_40)
LV_FONT_DECLARE(lv_font_montserrat_40)
#endif
#if defined(CONFIG_LV_FONT_MONTSERRAT_48) || defined(LV_FONT_MONTSERRAT_48)
LV_FONT_DECLARE(lv_font_montserrat_48)
#endif

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

#define UI_SCR_BG       0x0a0e0c
#define UI_TEXT_MAIN    0xd8ebe4
#define UI_ME_ACCENT    0x2ec4a0

static lv_obj_t* s_view;
static lv_obj_t* s_lbl_brand;
static lv_obj_t* s_lbl_clock;

static const lv_font_t* big_font(void) {
#if defined(CONFIG_LV_FONT_MONTSERRAT_48) || defined(LV_FONT_MONTSERRAT_48)
  return &lv_font_montserrat_48;
#elif defined(CONFIG_LV_FONT_MONTSERRAT_40) || defined(LV_FONT_MONTSERRAT_40)
  return &lv_font_montserrat_40;
#else
#ifdef BBCLAW_HAVE_CJK_FONT
  return &lv_font_bbclaw_cjk;
#else
  return lv_font_get_default();
#endif
#endif
}

void bb_page_standby_create(lv_obj_t* scr) {
  if (scr == NULL || s_view != NULL) return;

  s_view = lv_obj_create(scr);
  lv_obj_remove_style_all(s_view);
  lv_obj_set_size(s_view, DISP_W, DISP_H);
  lv_obj_set_pos(s_view, 0, 0);
  lv_obj_set_style_bg_color(s_view, lv_color_hex(UI_SCR_BG), 0);
  lv_obj_set_style_bg_opa(s_view, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_view, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_set_scrollbar_mode(s_view, LV_SCROLLBAR_MODE_OFF);

  const lv_font_t* font = big_font();
  const int lh = (int)lv_font_get_line_height(font);

  /* "BBClaw" brand — top third */
  const int brand_y = (DISP_H / 3) - (lh / 2);
  s_lbl_brand = lv_label_create(s_view);
  lv_obj_set_style_text_color(s_lbl_brand, lv_color_hex(UI_ME_ACCENT), 0);
  lv_obj_set_style_text_font(s_lbl_brand, font, 0);
  lv_obj_set_style_text_align(s_lbl_brand, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_lbl_brand, DISP_W);
  lv_label_set_text(s_lbl_brand, "BBClaw");
  lv_obj_set_pos(s_lbl_brand, 0, brand_y);

  /* Clock — bottom 2/3 */
  const int clock_y = (DISP_H * 2 / 3) - (lh / 2) + 8;
  s_lbl_clock = lv_label_create(s_view);
  lv_obj_set_style_text_color(s_lbl_clock, lv_color_hex(UI_TEXT_MAIN), 0);
  lv_obj_set_style_text_font(s_lbl_clock, font, 0);
  lv_obj_set_style_text_align(s_lbl_clock, LV_TEXT_ALIGN_CENTER, 0);
  lv_obj_set_width(s_lbl_clock, DISP_W);
  lv_label_set_text(s_lbl_clock, "--:--");
  lv_obj_set_pos(s_lbl_clock, 0, clock_y);
}

void bb_page_standby_set_visible(int visible) {
  if (s_view == NULL) return;
  if (visible) lv_obj_clear_flag(s_view, LV_OBJ_FLAG_HIDDEN);
  else lv_obj_add_flag(s_view, LV_OBJ_FLAG_HIDDEN);
}

void bb_page_standby_refresh_clock(const char* hm) {
  if (s_lbl_clock == NULL || hm == NULL) return;
  lv_label_set_text(s_lbl_clock, hm);
}
