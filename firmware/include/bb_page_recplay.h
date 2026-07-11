#pragma once

#include "lvgl.h"

/**
 * bb_page_recplay — 录音回放页（ADR-044 P1a：设备端「正在播放」媒体页）。
 *
 * 触屏手表（AMOLED-2.06,原生 LVGL 指针 indev）上，点 Recordings 列表某条录音
 * 即打开本页并连播整段会话。页内 transport 全触控：上一段 / 播放·暂停 / 下一段 /
 * 停止 + 可拖动音量条。右滑(BACK) 退回列表。
 *
 * 播放引擎见 bb_recplay（会话连播 + 暂停 + 跳段）。本页只做 UI + 调 transport,
 * 250ms lv_timer 轮询 bb_recplay_get_state 刷新图标/进度。
 *
 * 生命周期：由 bb_ui_settings 的 LEVEL_RECPLAY 层持有——open 建页并起播,
 * close 停播并拆页。音量为全局量,本页只改 live 值(bb_audio_set_volume_pct)并记
 * dirty,持久化由 settings 在离开时走既有 off-thread 路径(避免 stream_task
 * PSRAM 栈上写 NVS 的 cache-freeze)。所有调用须在 LVGL 锁内。
 */

/** 建页 + 起播录音。parent=承载容器(settings root);start_idx=录音列表下标
 *  (newest-first,见 bb_ui_settings_recfiles_session)。页内「上一首/下一首」按该
 *  列表在录音之间切换(不是段级——一次录音=一首)。 */
void bb_page_recplay_open(lv_obj_t* parent, int start_idx);

/** 停播 + 拆页（幂等）。 */
void bb_page_recplay_close(void);

/** 1 = 本页已建。 */
int bb_page_recplay_is_open(void);

/* ── 按键降级（bench/无触屏板:rotate→音量, OK→播放暂停）── */
void bb_page_recplay_key_toggle(void);  /* OK: 播放 / 暂停 切换 */
void bb_page_recplay_key_volume(int delta); /* rotate: 音量 ±步进 */
void bb_page_recplay_key_prev(void);    /* LEFT: 上一首(录音) */
void bb_page_recplay_key_next(void);    /* RIGHT: 下一首(录音) */

/* ── 音量交回 settings 持久化 ── */
int bb_page_recplay_volume_pct(void);   /* 当前 live 音量 */
int bb_page_recplay_volume_dirty(void); /* 1 = 本页改过音量,离开时需持久化 */
