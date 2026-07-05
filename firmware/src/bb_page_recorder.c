/**
 * bb_page_recorder.c — 录音模式常显页（ADR-044 §3.6，合规刚性指示）。
 *
 * 构图（410x502 竖屏优先，方屏退化为同构小版面）：
 *   中上：呼吸红点 + REC 字样
 *   中央：已录时长 HH:MM:SS（大号）
 *   下方：段数 · 书签数 · SD 剩余；写错误时红字告警
 *   底部：退出提示（BACK 第一按后显示「再滑一次停止录音」）
 *
 * 挂 lv_layer_top：盖住底下 chat/standby 视图；1s lv_timer 刷新状态。
 */
#include "bb_page_recorder.h"

#include <stdio.h>

#include "bb_config.h"
#include "bb_recorder.h"
#include "bb_ui_layout.h"
#include "bb_ui_theme.h"
#include "lvgl.h"

#define DISP_W BBCLAW_ST7789_WIDTH
#define DISP_H BBCLAW_ST7789_HEIGHT

#define REC_RED 0xE05A5A

static lv_obj_t* s_root;
static lv_obj_t* s_dot;
static lv_obj_t* s_lbl_elapsed;
static lv_obj_t* s_lbl_stats;
static lv_obj_t* s_lbl_warn;
static lv_obj_t* s_lbl_exit_hint;
static lv_timer_t* s_timer;

static void dot_pulse_cb(void* obj, int32_t v) {
  lv_obj_set_style_bg_opa((lv_obj_t*)obj, (lv_opa_t)v, 0);
}

static void refresh_cb(lv_timer_t* t) {
  (void)t;
  bb_recorder_status_t st = {0};
  bb_recorder_get_status(&st);

  const int64_t s = st.elapsed_ms / 1000;
  char buf[32];
  snprintf(buf, sizeof(buf), "%02d:%02d:%02d", (int)(s / 3600), (int)((s / 60) % 60), (int)(s % 60));
  lv_label_set_text(s_lbl_elapsed, buf);

  char stats[96];
  if (st.sd_free_kb > 0) {
    snprintf(stats, sizeof(stats), "%d seg · %d mark · SD %.1fGB free", st.segment_count,
             st.bookmark_count, (double)st.sd_free_kb / (1024.0 * 1024.0));
  } else {
    snprintf(stats, sizeof(stats), "%d seg · %d mark", st.segment_count, st.bookmark_count);
  }
  lv_label_set_text(s_lbl_stats, stats);

  if (st.write_error) {
    lv_label_set_text(s_lbl_warn, "SD write error!");
    lv_obj_clear_flag(s_lbl_warn, LV_OBJ_FLAG_HIDDEN);
  } else if (!st.active) {
    lv_label_set_text(s_lbl_warn, "stopped");
    lv_obj_clear_flag(s_lbl_warn, LV_OBJ_FLAG_HIDDEN);
  } else {
    lv_obj_add_flag(s_lbl_warn, LV_OBJ_FLAG_HIDDEN);
  }
}

void bb_page_recorder_show(void) {
  if (s_root != NULL) return;

  s_root = lv_obj_create(lv_layer_top());
  lv_obj_remove_style_all(s_root);
  lv_obj_set_size(s_root, DISP_W, DISP_H);
  lv_obj_set_pos(s_root, 0, 0);
  lv_obj_set_style_bg_color(s_root, lv_color_hex(BB_UI_BG), 0);
  lv_obj_set_style_bg_opa(s_root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_root, LV_OBJ_FLAG_SCROLLABLE);

  /* 呼吸红点 + REC */
  s_dot = lv_obj_create(s_root);
  lv_obj_remove_style_all(s_dot);
  lv_obj_set_size(s_dot, 18, 18);
  lv_obj_set_style_radius(s_dot, LV_RADIUS_CIRCLE, 0);
  lv_obj_set_style_bg_color(s_dot, lv_color_hex(REC_RED), 0);
  lv_obj_set_style_bg_opa(s_dot, LV_OPA_COVER, 0);
  lv_obj_align(s_dot, LV_ALIGN_TOP_MID, -28, DISP_H / 5);

  lv_obj_t* rec = lv_label_create(s_root);
  lv_label_set_text(rec, "REC");
  lv_obj_set_style_text_color(rec, lv_color_hex(REC_RED), 0);
  lv_obj_align(rec, LV_ALIGN_TOP_MID, 14, DISP_H / 5 - 1);

  lv_anim_t a;
  lv_anim_init(&a);
  lv_anim_set_var(&a, s_dot);
  lv_anim_set_values(&a, LV_OPA_20, LV_OPA_COVER);
  lv_anim_set_duration(&a, 900);
  lv_anim_set_playback_duration(&a, 900);
  lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
  lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
  lv_anim_set_exec_cb(&a, dot_pulse_cb);
  lv_anim_start(&a);

  /* 时长（大号；无 CJK 依赖,纯数字用默认字体放大 transform） */
  s_lbl_elapsed = lv_label_create(s_root);
  lv_label_set_text(s_lbl_elapsed, "00:00:00");
  lv_obj_set_style_text_color(s_lbl_elapsed, lv_color_hex(BB_UI_DOT_LIT), 0);
  lv_obj_set_style_transform_scale(s_lbl_elapsed, 512, 0); /* 2x */
  lv_obj_set_style_transform_pivot_x(s_lbl_elapsed, LV_PCT(50), 0);
  lv_obj_set_style_transform_pivot_y(s_lbl_elapsed, LV_PCT(50), 0);
  lv_obj_align(s_lbl_elapsed, LV_ALIGN_CENTER, 0, -DISP_H / 20);

  s_lbl_stats = lv_label_create(s_root);
  lv_label_set_text(s_lbl_stats, "");
  lv_obj_set_style_text_color(s_lbl_stats, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_obj_align(s_lbl_stats, LV_ALIGN_CENTER, 0, DISP_H / 8);

  s_lbl_warn = lv_label_create(s_root);
  lv_label_set_text(s_lbl_warn, "");
  lv_obj_set_style_text_color(s_lbl_warn, lv_color_hex(BB_UI_ERR), 0);
  lv_obj_align(s_lbl_warn, LV_ALIGN_CENTER, 0, DISP_H / 8 + 28);
  lv_obj_add_flag(s_lbl_warn, LV_OBJ_FLAG_HIDDEN);

  s_lbl_exit_hint = lv_label_create(s_root);
  lv_label_set_text(s_lbl_exit_hint, "PTT = mark · Swipe right x2 = stop");
  lv_obj_set_style_text_color(s_lbl_exit_hint, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_obj_align(s_lbl_exit_hint, LV_ALIGN_BOTTOM_MID, 0, -BB_UI_SAFE_BOTTOM - 8);

  s_timer = lv_timer_create(refresh_cb, 1000, NULL);
  refresh_cb(NULL);
}

void bb_page_recorder_hide(void) {
  if (s_root == NULL) return;
  if (s_timer != NULL) {
    lv_timer_delete(s_timer);
    s_timer = NULL;
  }
  lv_anim_delete(s_dot, dot_pulse_cb);
  lv_obj_delete(s_root);
  s_root = NULL;
  s_dot = NULL;
  s_lbl_elapsed = NULL;
  s_lbl_stats = NULL;
  s_lbl_warn = NULL;
  s_lbl_exit_hint = NULL;
}

void bb_page_recorder_exit_hint(int arm) {
  if (s_lbl_exit_hint == NULL) return;
  lv_label_set_text(s_lbl_exit_hint,
                    arm ? "Swipe right again to STOP" : "PTT = mark · Swipe right x2 = stop");
  lv_obj_set_style_text_color(s_lbl_exit_hint,
                              lv_color_hex(arm ? REC_RED : BB_UI_TEXT_DIM), 0);
}
