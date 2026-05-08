#pragma once

#include "lvgl.h"

/* LOCKED page: independent full-screen padlock view.
 * Shown when cloud_saas passphrase unlock is required.
 * update_status() handles BB_STATUS_VERIFY_TX/VERIFY/VERIFY_ERR transitions. */

void bb_page_locked_create(lv_obj_t* scr);
void bb_page_locked_set_visible(int visible);
void bb_page_locked_update_status(const char* status);
