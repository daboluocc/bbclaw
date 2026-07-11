/**
 * bb_page_recplay.c — 录音回放页（ADR-044 P1a）。见 bb_page_recplay.h 头注。
 *
 * 构图（410x502 圆角屏，R114 大圆角——全宽元素只放中段 y∈[114,388]，贴底元素
 * 收窄居中）：
 *   顶部：录音标题（日期时间）
 *   中上：大号已播时长 M:SS  ·  段号 N/M  ·  总时长
 *   中：会话进度条（已播 / 总）
 *   中下：transport 三键 ⏮ ⏯ ⏭（触控圆钮）
 *   其下：停止 STOP 钮
 *   贴底：可拖动音量条
 *
 * 进度：页内 250ms lv_timer 自累计已播毫秒（播放中累加、暂停冻结），按 seg_cur
 * 对齐段边界基准（跳段时基准随段号跳变，自纠偏），故无需引擎报采样位置。
 */
#include "bb_page_recplay.h"

#include <stdio.h>
#include <string.h>

#include "bb_audio.h"
#include "bb_device_config.h"
#include "bb_recplay.h"
#include "bb_ui_layout.h"
#include "bb_ui_settings.h"
#include "bb_ui_theme.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "lvgl.h"

static const char* TAG = "bb_recplay_ui";

/* ── 几何（居中对齐 + 偏移，随屏宽自适应）── */
#define RP_BAR_W        330 /* 进度/音量条宽（贴底音量避开 R114 角） */
#define RP_BAR_H        10
#define RP_VOL_H        22
#define RP_BTN_MAIN     92 /* 播放/暂停中心钮直径 */
#define RP_BTN_SIDE     70 /* 上一/下一钮直径 */
#define RP_BTN_GAP      118 /* 中心钮到侧钮的水平间距 */
#define RP_STOP_W       132
#define RP_STOP_H       46
#define RP_VOL_STEP     5  /* 按键降级每步音量 */

typedef struct {
  lv_obj_t* root;
  lv_obj_t* title_lbl;
  lv_obj_t* time_lbl;   /* 大号已播时长 */
  lv_obj_t* meta_lbl;   /* "N/M · 总时长" */
  lv_obj_t* prog_fill;
  lv_obj_t* playpause_lbl; /* 中心钮里的符号 */
  lv_obj_t* vol_fill;
  lv_obj_t* vol_lbl;
  lv_timer_t* timer;

  char dir[96];
  int cur_idx;          /* 当前录音在列表中的下标(newest-first) */
  int total_s;          /* 会话总时长秒（<=0 未知） */

  int vol_pct;
  int vol_dirty;

  /* 已播进度累计 */
  int last_seg;
  int64_t seg_accum_ms;
  int64_t last_tick_us;
} rp_state_t;

static rp_state_t s_rp;

/* ── 进度累计 ── */
static int rp_elapsed_ms(int seg_cur, int seg_total) {
  if (s_rp.total_s <= 0 || seg_total <= 0 || seg_cur <= 0) {
    return 0;
  }
  int64_t total_ms = (int64_t)s_rp.total_s * 1000;
  int64_t seg_len_ms = total_ms / seg_total;
  int64_t base_ms = (total_ms * (seg_cur - 1)) / seg_total;
  int64_t within = s_rp.seg_accum_ms;
  if (within > seg_len_ms) within = seg_len_ms;
  int64_t e = base_ms + within;
  if (e > total_ms) e = total_ms;
  return (int)e;
}

static void rp_fmt_mmss(int secs, char* buf, size_t n) {
  if (secs < 0) secs = 0;
  snprintf(buf, n, "%d:%02d", secs / 60, secs % 60);
}

static void rp_set_prog(int permille) {
  if (s_rp.prog_fill == NULL) return;
  if (permille < 0) permille = 0;
  if (permille > 1000) permille = 1000;
  lv_coord_t inner = RP_BAR_W - 4;
  lv_obj_set_width(s_rp.prog_fill, (lv_coord_t)(permille * inner / 1000));
}

static void rp_set_vol(int pct) {
  if (pct < 0) pct = 0;
  if (pct > 100) pct = 100;
  if (s_rp.vol_fill != NULL) {
    lv_coord_t inner = RP_BAR_W - 4;
    lv_obj_set_width(s_rp.vol_fill, (lv_coord_t)(pct * (int)inner / 100));
  }
  if (s_rp.vol_lbl != NULL) {
    char buf[24];
    snprintf(buf, sizeof(buf), LV_SYMBOL_VOLUME_MAX "  %d%%", pct);
    lv_label_set_text(s_rp.vol_lbl, buf);
  }
}

/* ── 250ms 刷新：图标 / 时长 / 段号 / 进度条 ── */
static void rp_tick(lv_timer_t* t) {
  (void)t;
  bb_recplay_state_t st = {0};
  bb_recplay_get_state(&st);

  int64_t now = esp_timer_get_time();
  int64_t dt = (s_rp.last_tick_us > 0) ? (now - s_rp.last_tick_us) : 0;
  s_rp.last_tick_us = now;

  if (st.active) {
    if (st.seg_cur != s_rp.last_seg) {
      s_rp.last_seg = st.seg_cur;
      s_rp.seg_accum_ms = 0; /* 段边界（含跳段）重置段内累计 */
    }
    if (!st.paused && dt > 0) {
      s_rp.seg_accum_ms += dt / 1000;
    }
  }

  /* 播放/暂停图标：播放中=⏸ 可暂停;暂停或已停=▶ 可(继续/重放) */
  if (s_rp.playpause_lbl != NULL) {
    const char* sym = (st.active && !st.paused) ? LV_SYMBOL_PAUSE : LV_SYMBOL_PLAY;
    lv_label_set_text(s_rp.playpause_lbl, sym);
  }

  int seg_cur = st.seg_cur, seg_total = st.seg_total;
  int elapsed = st.active ? rp_elapsed_ms(seg_cur, seg_total) : (s_rp.total_s * 1000);
  if (!st.active) {
    /* 播完/停止：进度拉满或归零依 seg。已停但未播过 → 0。 */
    elapsed = (s_rp.last_seg > 0) ? s_rp.total_s * 1000 : 0;
  }

  if (s_rp.time_lbl != NULL) {
    char buf[16];
    rp_fmt_mmss(elapsed / 1000, buf, sizeof(buf));
    lv_label_set_text(s_rp.time_lbl, buf);
  }
  if (s_rp.meta_lbl != NULL) {
    char buf[48];
    char tot[16];
    rp_fmt_mmss(s_rp.total_s, tot, sizeof(tot));
    /* 段计数只对多段(长)录音有意义;单段/已停只显总时长。分隔符用 ASCII "|"
     * (「·」不在 montserrat 字库,真机显方框——与录音页同坑)。 */
    if (seg_total > 1) {
      snprintf(buf, sizeof(buf), "%d/%d  |  %s", seg_cur > 0 ? seg_cur : 1, seg_total, tot);
    } else {
      snprintf(buf, sizeof(buf), "%s", tot);
    }
    lv_label_set_text(s_rp.meta_lbl, buf);
  }
  int total_ms = s_rp.total_s * 1000;
  rp_set_prog(total_ms > 0 ? (int)((int64_t)elapsed * 1000 / total_ms) : 0);
}

/* ── transport 按钮回调 ── */
static void rp_on_playpause(lv_event_t* e) {
  (void)e;
  bb_recplay_state_t st = {0};
  bb_recplay_get_state(&st);
  if (!st.active) {
    /* 已停：从头重放整段会话 */
    s_rp.last_seg = 0;
    s_rp.seg_accum_ms = 0;
    (void)bb_recplay_toggle_session(s_rp.dir);
  } else {
    bb_recplay_set_paused(!st.paused);
  }
  rp_tick(NULL);
}

/* 切到列表第 idx 条录音并起播（「上一首/下一首」= 换录音,不是段级跳）。 */
static void rp_load_session(int idx) {
  char dir[96];
  char title[24];
  int total_s = 0;
  if (!bb_ui_settings_recfiles_session(idx, dir, sizeof(dir), title, sizeof(title), &total_s)) {
    return;
  }
  bb_recplay_stop(); /* 停当前录音(阻塞至收尾,毫秒级) */
  s_rp.cur_idx = idx;
  snprintf(s_rp.dir, sizeof(s_rp.dir), "%s", dir);
  s_rp.total_s = total_s;
  s_rp.last_seg = 0;
  s_rp.seg_accum_ms = 0;
  if (s_rp.title_lbl != NULL) lv_label_set_text(s_rp.title_lbl, title);
  (void)bb_recplay_toggle_session(s_rp.dir);
  rp_tick(NULL);
  ESP_LOGI(TAG, "load session idx=%d %s", idx, s_rp.dir);
}

static void rp_on_prev(lv_event_t* e) {
  (void)e;
  /* 上一首 = 列表里更新的一条(idx-1);已在最新则重放当前。 */
  if (s_rp.cur_idx > 0) {
    rp_load_session(s_rp.cur_idx - 1);
  } else {
    rp_load_session(s_rp.cur_idx); /* 重放当前 */
  }
}

static void rp_on_next(lv_event_t* e) {
  (void)e;
  /* 下一首 = 列表里更旧的一条(idx+1);已在最旧则不动。 */
  int n = bb_ui_settings_recfiles_count();
  if (s_rp.cur_idx + 1 < n) {
    rp_load_session(s_rp.cur_idx + 1);
  }
}

static void rp_on_stop(lv_event_t* e) {
  (void)e;
  bb_recplay_stop();
  s_rp.last_seg = 0;
  s_rp.seg_accum_ms = 0;
  rp_tick(NULL);
}

/* ── 音量拖动（贴底条）—— 复刻 settings vol_bar_touch_cb 坐标算法 ── */
static void rp_on_vol(lv_event_t* e) {
  lv_event_code_t code = lv_event_get_code(e);
  if (code == LV_EVENT_PRESSED || code == LV_EVENT_PRESSING) {
    lv_indev_t* indev = lv_event_get_indev(e);
    if (indev == NULL) return;
    lv_point_t p;
    lv_indev_get_point(indev, &p);
    lv_obj_t* bar = (lv_obj_t*)lv_event_get_current_target(e);
    lv_area_t c;
    lv_obj_get_coords(bar, &c);
    int inner = (int)lv_area_get_width(&c) - 4;
    if (inner <= 0) return;
    int v = (((int)p.x - (int)c.x1 - 2) * 100) / inner;
    if (v < 0) v = 0;
    if (v > 100) v = 100;
    if (v == s_rp.vol_pct) return;
    s_rp.vol_pct = v;
    s_rp.vol_dirty = 1;
    bb_audio_set_volume_pct(v);
    rp_set_vol(v);
  }
  /* 持久化交由 settings 离开 LEVEL_RECPLAY 时统一走 off-thread 路径 */
}

/* ── 建钮辅助 ── */
static lv_obj_t* rp_make_btn(lv_obj_t* parent, int dia, uint32_t border, lv_event_cb_t cb,
                             lv_obj_t** out_lbl, const char* sym) {
  lv_obj_t* b = lv_obj_create(parent);
  lv_obj_remove_style_all(b);
  lv_obj_set_size(b, dia, dia);
  lv_obj_set_style_radius(b, LV_RADIUS_CIRCLE, 0);
  lv_obj_set_style_bg_color(b, lv_color_hex(BB_UI_DOT_GHOST), 0);
  lv_obj_set_style_bg_opa(b, LV_OPA_COVER, 0);
  lv_obj_set_style_border_width(b, 2, 0);
  lv_obj_set_style_border_color(b, lv_color_hex(border), 0);
  lv_obj_set_style_border_opa(b, LV_OPA_COVER, 0);
  lv_obj_clear_flag(b, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_add_flag(b, LV_OBJ_FLAG_CLICKABLE);
  lv_obj_add_event_cb(b, cb, LV_EVENT_CLICKED, NULL);
  lv_obj_t* l = lv_label_create(b);
  lv_label_set_text(l, sym);
  lv_obj_set_style_text_font(l, &lv_font_montserrat_40, 0);
  lv_obj_set_style_text_color(l, lv_color_hex(BB_UI_DOT_LIT), 0);
  lv_obj_center(l);
  if (out_lbl != NULL) *out_lbl = l;
  return b;
}

void bb_page_recplay_open(lv_obj_t* parent, int start_idx) {
  if (parent == NULL) return;
  char dir[96];
  char title[24];
  int total_s = 0;
  if (!bb_ui_settings_recfiles_session(start_idx, dir, sizeof(dir), title, sizeof(title), &total_s)) {
    return;
  }
  if (s_rp.root != NULL) bb_page_recplay_close();

  memset(&s_rp, 0, sizeof(s_rp));
  snprintf(s_rp.dir, sizeof(s_rp.dir), "%s", dir);
  s_rp.cur_idx = start_idx;
  s_rp.total_s = total_s;
  s_rp.vol_pct = bb_device_config_get()->volume_pct;
  s_rp.last_seg = 0;

  s_rp.root = lv_obj_create(parent);
  lv_obj_remove_style_all(s_rp.root);
  lv_obj_set_size(s_rp.root, lv_pct(100), lv_pct(100));
  lv_obj_set_style_bg_color(s_rp.root, lv_color_hex(BB_UI_BG), 0);
  lv_obj_set_style_bg_opa(s_rp.root, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_rp.root, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_move_foreground(s_rp.root);

  /* 标题（日期时间），顶部居中避开圆角 */
  s_rp.title_lbl = lv_label_create(s_rp.root);
  lv_label_set_text(s_rp.title_lbl, title[0] != '\0' ? title : "Recording");
  lv_obj_set_style_text_font(s_rp.title_lbl, &lv_font_montserrat_14, 0);
  lv_obj_set_style_text_color(s_rp.title_lbl, lv_color_hex(BB_UI_DOT_LIT), 0);
  lv_obj_align(s_rp.title_lbl, LV_ALIGN_TOP_MID, 0, BB_UI_SAFE_TOP + 8);

  /* 大号已播时长 */
  s_rp.time_lbl = lv_label_create(s_rp.root);
  lv_label_set_text(s_rp.time_lbl, "0:00");
  lv_obj_set_style_text_font(s_rp.time_lbl, &lv_font_montserrat_40, 0);
  lv_obj_set_style_text_color(s_rp.time_lbl, lv_color_hex(BB_UI_DOT_LIT), 0);
  lv_obj_align(s_rp.time_lbl, LV_ALIGN_CENTER, 0, -108);

  /* 段号 · 总时长 */
  s_rp.meta_lbl = lv_label_create(s_rp.root);
  lv_label_set_text(s_rp.meta_lbl, "");
  lv_obj_set_style_text_font(s_rp.meta_lbl, &lv_font_montserrat_14, 0);
  lv_obj_set_style_text_color(s_rp.meta_lbl, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_obj_align(s_rp.meta_lbl, LV_ALIGN_CENTER, 0, -64);

  /* 进度条（track + fill），中段全宽安全 */
  lv_obj_t* track = lv_obj_create(s_rp.root);
  lv_obj_remove_style_all(track);
  lv_obj_set_size(track, RP_BAR_W, RP_BAR_H);
  lv_obj_set_style_radius(track, RP_BAR_H / 2, 0);
  lv_obj_set_style_bg_color(track, lv_color_hex(BB_UI_DOT_GHOST), 0);
  lv_obj_set_style_bg_opa(track, LV_OPA_COVER, 0);
  lv_obj_clear_flag(track, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_clear_flag(track, LV_OBJ_FLAG_CLICKABLE);
  lv_obj_align(track, LV_ALIGN_CENTER, 0, -30);
  s_rp.prog_fill = lv_obj_create(track);
  lv_obj_remove_style_all(s_rp.prog_fill);
  lv_obj_set_size(s_rp.prog_fill, 0, RP_BAR_H - 4);
  lv_obj_set_pos(s_rp.prog_fill, 2, 2);
  lv_obj_set_style_radius(s_rp.prog_fill, (RP_BAR_H - 4) / 2, 0);
  lv_obj_set_style_bg_color(s_rp.prog_fill, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_rp.prog_fill, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_rp.prog_fill, LV_OBJ_FLAG_CLICKABLE);

  /* transport 三键 */
  lv_obj_t* prev = rp_make_btn(s_rp.root, RP_BTN_SIDE, BB_UI_TEXT_DIM, rp_on_prev, NULL, LV_SYMBOL_PREV);
  lv_obj_align(prev, LV_ALIGN_CENTER, -RP_BTN_GAP, 46);
  lv_obj_t* play = rp_make_btn(s_rp.root, RP_BTN_MAIN, BB_UI_ACCENT, rp_on_playpause,
                               &s_rp.playpause_lbl, LV_SYMBOL_PLAY);
  lv_obj_align(play, LV_ALIGN_CENTER, 0, 46);
  lv_obj_t* next = rp_make_btn(s_rp.root, RP_BTN_SIDE, BB_UI_TEXT_DIM, rp_on_next, NULL, LV_SYMBOL_NEXT);
  lv_obj_align(next, LV_ALIGN_CENTER, RP_BTN_GAP, 46);

  /* 停止钮 */
  lv_obj_t* stop = lv_obj_create(s_rp.root);
  lv_obj_remove_style_all(stop);
  lv_obj_set_size(stop, RP_STOP_W, RP_STOP_H);
  lv_obj_set_style_radius(stop, RP_STOP_H / 2, 0);
  lv_obj_set_style_bg_color(stop, lv_color_hex(BB_UI_DOT_GHOST), 0);
  lv_obj_set_style_bg_opa(stop, LV_OPA_COVER, 0);
  lv_obj_set_style_border_width(stop, 2, 0);
  lv_obj_set_style_border_color(stop, lv_color_hex(BB_UI_ERR), 0);
  lv_obj_set_style_border_opa(stop, LV_OPA_COVER, 0);
  lv_obj_clear_flag(stop, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_add_flag(stop, LV_OBJ_FLAG_CLICKABLE);
  lv_obj_add_event_cb(stop, rp_on_stop, LV_EVENT_CLICKED, NULL);
  lv_obj_align(stop, LV_ALIGN_CENTER, 0, 130);
  lv_obj_t* stop_l = lv_label_create(stop);
  lv_label_set_text(stop_l, LV_SYMBOL_STOP "  STOP");
  lv_obj_set_style_text_font(stop_l, &lv_font_montserrat_14, 0);
  lv_obj_set_style_text_color(stop_l, lv_color_hex(BB_UI_ERR), 0);
  lv_obj_center(stop_l);

  /* 音量：% 标签 + 可拖动条（贴底但避 R114 角，收窄居中） */
  s_rp.vol_lbl = lv_label_create(s_rp.root);
  lv_obj_set_style_text_font(s_rp.vol_lbl, &lv_font_montserrat_14, 0);
  lv_obj_set_style_text_color(s_rp.vol_lbl, lv_color_hex(BB_UI_TEXT_DIM), 0);
  lv_obj_align(s_rp.vol_lbl, LV_ALIGN_BOTTOM_MID, 0, -(BB_UI_SAFE_BOTTOM + RP_VOL_H + 14));

  lv_obj_t* volbar = lv_obj_create(s_rp.root);
  lv_obj_remove_style_all(volbar);
  lv_obj_set_size(volbar, RP_BAR_W, RP_VOL_H);
  lv_obj_set_style_radius(volbar, RP_VOL_H / 2, 0);
  lv_obj_set_style_bg_color(volbar, lv_color_hex(BB_UI_DOT_GHOST), 0);
  lv_obj_set_style_bg_opa(volbar, LV_OPA_COVER, 0);
  lv_obj_clear_flag(volbar, LV_OBJ_FLAG_SCROLLABLE);
  lv_obj_clear_flag(volbar, LV_OBJ_FLAG_GESTURE_BUBBLE); /* 横拖不冒泡成右滑=BACK */
  lv_obj_set_ext_click_area(volbar, 14);
  lv_obj_align(volbar, LV_ALIGN_BOTTOM_MID, 0, -BB_UI_SAFE_BOTTOM);
  lv_obj_add_event_cb(volbar, rp_on_vol, LV_EVENT_PRESSED, NULL);
  lv_obj_add_event_cb(volbar, rp_on_vol, LV_EVENT_PRESSING, NULL);
  s_rp.vol_fill = lv_obj_create(volbar);
  lv_obj_remove_style_all(s_rp.vol_fill);
  lv_obj_set_size(s_rp.vol_fill, 0, RP_VOL_H - 4);
  lv_obj_set_pos(s_rp.vol_fill, 2, 2);
  lv_obj_set_style_radius(s_rp.vol_fill, (RP_VOL_H - 4) / 2, 0);
  lv_obj_set_style_bg_color(s_rp.vol_fill, lv_color_hex(BB_UI_ACCENT), 0);
  lv_obj_set_style_bg_opa(s_rp.vol_fill, LV_OPA_COVER, 0);
  lv_obj_clear_flag(s_rp.vol_fill, LV_OBJ_FLAG_CLICKABLE);

  rp_set_vol(s_rp.vol_pct);

  /* 起播整段会话 */
  esp_err_t err = bb_recplay_toggle_session(s_rp.dir);
  if (err == ESP_ERR_INVALID_STATE) {
    lv_label_set_text(s_rp.meta_lbl, "Busy (recording/TTS)");
  }

  s_rp.timer = lv_timer_create(rp_tick, 250, NULL);
  rp_tick(NULL);
  ESP_LOGI(TAG, "open %s total=%ds", s_rp.dir, s_rp.total_s);
}

void bb_page_recplay_close(void) {
  if (s_rp.root == NULL) return;
  if (s_rp.timer != NULL) {
    lv_timer_delete(s_rp.timer);
    s_rp.timer = NULL;
  }
  bb_recplay_stop();
  lv_obj_delete(s_rp.root);
  s_rp.root = NULL;
  s_rp.title_lbl = NULL;
  s_rp.time_lbl = NULL;
  s_rp.meta_lbl = NULL;
  s_rp.prog_fill = NULL;
  s_rp.playpause_lbl = NULL;
  s_rp.vol_fill = NULL;
  s_rp.vol_lbl = NULL;
  ESP_LOGI(TAG, "close");
}

int bb_page_recplay_is_open(void) { return s_rp.root != NULL ? 1 : 0; }

void bb_page_recplay_key_toggle(void) {
  if (s_rp.root == NULL) return;
  rp_on_playpause(NULL);
}

void bb_page_recplay_key_prev(void) {
  if (s_rp.root == NULL) return;
  rp_on_prev(NULL);
}

void bb_page_recplay_key_next(void) {
  if (s_rp.root == NULL) return;
  rp_on_next(NULL);
}

void bb_page_recplay_key_volume(int delta) {
  if (s_rp.root == NULL || delta == 0) return;
  int v = s_rp.vol_pct + (delta > 0 ? RP_VOL_STEP : -RP_VOL_STEP);
  if (v < 0) v = 0;
  if (v > 100) v = 100;
  if (v == s_rp.vol_pct) return;
  s_rp.vol_pct = v;
  s_rp.vol_dirty = 1;
  bb_audio_set_volume_pct(v);
  rp_set_vol(v);
}

int bb_page_recplay_volume_pct(void) { return s_rp.vol_pct; }
int bb_page_recplay_volume_dirty(void) { return s_rp.vol_dirty; }
