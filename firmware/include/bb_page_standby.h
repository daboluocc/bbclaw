#pragma once

#include "lvgl.h"

/* STANDBY page: independent full-screen view, no top/bottom bar.
 * Shows "BBClaw" brand text + large clock.
 * Called by bb_display.c during bb_display_init and refresh_ui. */

void bb_page_standby_create(lv_obj_t* scr);
void bb_page_standby_set_visible(int visible);
void bb_page_standby_refresh_clock(const char* hm);
