#pragma once

#include "lvgl.h"

/* LOCKED page: independent full-screen padlock view.
 * Shown when cloud_saas passphrase unlock is required.
 * update_status() handles BB_STATUS_VERIFY_TX/VERIFY/VERIFY_ERR transitions.
 * update_battery() refreshes the large battery widget below the padlock. */

void bb_page_locked_create(lv_obj_t* scr);
void bb_page_locked_set_visible(int visible);
void bb_page_locked_update_status(const char* status);
/** ADR-038: after a failed unlock, replace the hint with what the ASR heard
 *  ("听到「<heard>」请重说") so the user can adjust. Empty/NULL → no change (keeps
 *  the default error hint). Call right after update_status(BB_STATUS_VERIFY_ERR). */
void bb_page_locked_show_heard(const char* heard);
void bb_page_locked_update_battery(int supported, int available, int percent, int low, int charging);
