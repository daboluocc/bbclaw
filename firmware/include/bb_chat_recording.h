#pragma once

#include <stdint.h>
#include "lvgl.h"

/**
 * CHAT recording overlay — semi-transparent mask over transcript during PTT.
 * Shows mic badge + "正在聆听" title + VU meter bars + "松开发送" hint.
 *
 * Lifecycle:
 *   bb_chat_recording_create()  — called from theme on_enter (LVGL task)
 *   bb_chat_recording_show()    — called when PTT pressed (LVGL task)
 *   bb_chat_recording_hide()    — called when PTT released (LVGL task)
 *   bb_chat_recording_set_level() — called from audio task (thread-safe)
 *   bb_chat_recording_destroy() — called from theme on_exit (LVGL task)
 */

void bb_chat_recording_create(lv_obj_t* parent, int width, int height_px);
void bb_chat_recording_show(void);
void bb_chat_recording_hide(void);
void bb_chat_recording_set_level(uint8_t level_pct, int voiced);
void bb_chat_recording_destroy(void);
