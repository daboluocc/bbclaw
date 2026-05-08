#pragma once

#include <stdint.h>
#include "lvgl.h"

/**
 * CHAT recording overlay — semi-transparent mask over transcript during PTT.
 * Shows mic badge + "正在聆听" title + VU meter bars + "松开发送" hint.
 *
 * Phase 5 skeleton. Real implementation is currently in bb_lvgl_display.c
 * (s_view_speaking + record_timer_cb). Will be fully extracted during Phase 7
 * when the CHAT page takes ownership of the body container.
 */

void bb_chat_recording_create(lv_obj_t* parent, int width, int height_px);
void bb_chat_recording_show(void);
void bb_chat_recording_hide(void);
void bb_chat_recording_set_level(uint8_t level_pct, int voiced);
