#pragma once

#include "esp_err.h"
#include "lvgl.h"

/**
 * CHAT pickers: session / CWD / driver selection overlays.
 *
 * Phase 6 skeleton. Real implementation is currently in bb_ui_agent_chat.c
 * (session_picker_*, cwd_picker_*, cycle_driver). This file will own the
 * picker UI rendering after Phase 7; data fetch + NVS persistence stay in
 * bb_ui_agent_chat.c.
 *
 * Return values for bb_chat_picker_session_select():
 *   0 = session switched
 *   1 = user chose "Settings" row
 *   2 = user chose "+ 新建 session" (triggers CWD picker)
 *  -1 = picker not visible or invalid selection
 */

void bb_chat_picker_session_show(void);
void bb_chat_picker_session_hide(void);
void bb_chat_picker_session_move(int delta);
int  bb_chat_picker_session_select(void);
int  bb_chat_picker_session_is_visible(void);

void bb_chat_picker_cwd_show(void);
void bb_chat_picker_cwd_hide(void);
void bb_chat_picker_cwd_move(int delta);
void bb_chat_picker_cwd_confirm(void);
void bb_chat_picker_cwd_cancel(void);
int  bb_chat_picker_cwd_is_visible(void);

esp_err_t bb_chat_picker_cycle_driver(int delta);
