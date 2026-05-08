#pragma once

#include "lvgl.h"

/* CHAT bottom bar: session id (left) + cwd pool name (right).
 * Phase 3 skeleton; internal LVGL objects currently in bb_lvgl_display.c. */

void bb_chat_bottombar_create(lv_obj_t* parent, int y, int width);
void bb_chat_bottombar_set_session_id(const char* session_id);
void bb_chat_bottombar_set_cwd_name(const char* cwd_name);
