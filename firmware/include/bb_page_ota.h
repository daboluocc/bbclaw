/**
 * OTA page — dot-matrix firmware-update progress.
 *
 * Same visual language as bb_page_boot / bb_page_netconn (dot palette,
 * lv_layer_top, synchronous hard-cut show/dismiss): a row of dot cells fills
 * left→right as the new firmware downloads, the leading cell flashing teal,
 * with an "UPDATING" title, a "NN%" readout and the target version below.
 *
 * Shown right before bb_ota_download_and_flash() and fed by its progress
 * callback; switched to the "rebooting" phase on success, then the device
 * reboots into the new slot. Dismissed on failure so the normal UI resumes.
 *
 * Progress is published lock-free via bb_page_ota_set_progress() (called from
 * the OTA download task); an internal LVGL timer repaints — same weak-read
 * pattern as bb_page_netconn's SSID poll. show/dismiss take the port lock.
 */
#ifndef BB_PAGE_OTA_H
#define BB_PAGE_OTA_H

#ifdef __cplusplus
extern "C" {
#endif

/** Create the page on lv_layer_top() with progress at 0%. version may be NULL.
 *  No-op if already shown. */
void bb_page_ota_show(const char* version);

/** Publish download progress (0-100). Lock-free; safe to call from the OTA
 *  download task. The page's timer picks it up on the next tick. */
void bb_page_ota_set_progress(int percent);

/** Switch to the "download complete, rebooting" phase (all cells lit, readout
 *  shows REBOOTING). Call right before bb_ota_apply_update(). */
void bb_page_ota_set_done(void);

/** Destroy the page synchronously. Call on the OTA-failure path. */
void bb_page_ota_dismiss(void);

/** 1 while the page object exists. */
int bb_page_ota_active(void);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_OTA_H */
