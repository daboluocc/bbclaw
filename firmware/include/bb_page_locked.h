#pragma once

#include "lvgl.h"

/* LOCKED page: independent full-screen padlock view.
 * Shown when cloud_saas passphrase unlock is required.
 * update_status() handles BB_STATUS_VERIFY_TX/VERIFY/VERIFY_ERR transitions.
 * update_battery() refreshes the large battery widget below the padlock.
 * update_footer() refreshes the bottom status bar (ADR-021-firmware-ui §2). */

void bb_page_locked_create(lv_obj_t* scr);
void bb_page_locked_set_visible(int visible);
void bb_page_locked_update_status(const char* status);
void bb_page_locked_update_battery(int supported, int available, int percent, int low, int charging);
/** ADR-021-firmware-ui §2: refresh LOCKED page footer.
 *  cwd: butler workspace cwd (NULL/"" → shows "[B]")
 *  mem_inbox/mem_profile: -1 means unknown → "mem: ?" */
void bb_page_locked_update_footer(const char* cwd, int mem_inbox, int mem_profile);
