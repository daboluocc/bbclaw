#pragma once

#include "lvgl.h"

/* CHAT top bar: status icon / status text / WiFi / battery / clock.
 * Phase 3 skeleton; internal LVGL objects currently in bb_lvgl_display.c. */

void bb_chat_topbar_create(lv_obj_t* parent);
void bb_chat_topbar_set_status(const char* status);
void bb_chat_topbar_set_clock(const char* hm);
void bb_chat_topbar_set_cloud_mode(int is_cloud);
