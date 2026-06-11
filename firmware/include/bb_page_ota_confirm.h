/**
 * OTA confirm page — user-consent prompt before firmware update.
 *
 * Shown when a new firmware version is detected (cloud_saas mode only).
 * The user can press OK to start the update or BACK to skip it.
 * A 30-second countdown auto-dismisses with "skip" if no input is received,
 * so unattended devices are never stuck on this screen.
 *
 * Visual language matches bb_page_ota / bb_page_netconn (dot palette,
 * lv_layer_top, synchronous show/dismiss). Input is routed by bb_radio_app.c
 * via bb_page_ota_confirm_active() + bb_page_ota_confirm_handle_nav().
 *
 * All calls take the LVGL port lock internally; callers hold no lock.
 */
#ifndef BB_PAGE_OTA_CONFIRM_H
#define BB_PAGE_OTA_CONFIRM_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/** Callback fired when the user (or timeout) makes a decision.
 *  accept == 1  → user pressed OK, proceed with update.
 *  accept == 0  → user pressed BACK or 30 s elapsed, skip update. */
typedef void (*bb_page_ota_confirm_cb_t)(int accept);

/** Create the confirm page on lv_layer_top(). No-op if already shown.
 *  current_ver / new_ver: firmware version strings (may be NULL/"").
 *  size_bytes: download size; shown as KB or MB.
 *  cb: called exactly once when the decision is made. */
void bb_page_ota_confirm_show(const char* current_ver,
                              const char* new_ver,
                              uint32_t    size_bytes,
                              bb_page_ota_confirm_cb_t cb);

/** Destroy the page synchronously. No-op if never shown / already dismissed.
 *  Does NOT fire the callback. */
void bb_page_ota_confirm_dismiss(void);

/** 1 while the page object exists (use to gate nav routing). */
int bb_page_ota_confirm_active(void);

/** 1 if the 30 s countdown expired and the main loop should call
 *  bb_page_ota_confirm_handle_nav(0) to dismiss and fire the callback. */
int bb_page_ota_confirm_timed_out(void);

/** Route a nav key press to the confirm page.
 *  nav_ok == 1 → treat as OK (upgrade); nav_ok == 0 → treat as BACK (skip).
 *  No-op if the page is not active.
 *  Also call this when bb_page_ota_confirm_timed_out() returns 1. */
void bb_page_ota_confirm_handle_nav(int nav_ok);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_OTA_CONFIRM_H */
