#pragma once

#include "lvgl.h"

/* STANDBY page: independent full-screen view, no top/bottom bar.
 * Shows "BBClaw" brand text + large clock + battery widget.
 * Called by bb_display.c during bb_display_init and refresh_ui. */

void bb_page_standby_create(lv_obj_t* scr);
void bb_page_standby_set_visible(int visible);
void bb_page_standby_refresh_clock(const char* hm);
void bb_page_standby_update_battery(int supported, int available, int percent, int low, int charging);

/* Show/refresh the unread-reminder badge on the idle screen (ADR-021 §9.3).
 * count<=0 hides it. Fed from bb_notification_unread_count() by the display tick. */
void bb_page_standby_set_unread(int count);

/* Request the low-power three-dot ambient overlay. Thread-safe: callers only
 * publish the desired state; LVGL objects are updated by the page timer. */
void bb_page_standby_set_ambient(int enabled);
