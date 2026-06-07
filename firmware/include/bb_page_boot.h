/**
 * BOOT splash — dot-matrix "BBCLAW" wordmark, Nokia-style column reveal.
 * See design/STATE_MACHINE.md §3.5.
 *
 * Both calls take the LVGL port lock internally; callers hold no lock.
 */
#ifndef BB_PAGE_BOOT_H
#define BB_PAGE_BOOT_H

#ifdef __cplusplus
extern "C" {
#endif

/** Create the splash on lv_layer_top() and start the reveal animation.
 *  No-op if already shown. Call right after bb_display_init(). */
void bb_page_boot_show(void);

/** Destroy the splash synchronously (hard cut, no fade — a parent-opa fade
 *  would composite through a transient full-screen layer buffer and used to
 *  starve esp_wifi_init's internal DMA buffers). All splash resources are
 *  freed when this returns. No-op if never shown / already dismissed. */
void bb_page_boot_dismiss(void);

/** 1 while the splash object exists. */
int bb_page_boot_active(void);

/** 1 once the reveal animation (sweep + underline) has fully played out —
 *  also 1 when no splash exists. The sweep runs on an lv_timer in the LVGL
 *  task, which boot-time audio init/playback can starve, so wall-clock waits
 *  alone may fire before the last columns render; poll this before dismiss. */
int bb_page_boot_anim_done(void);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_BOOT_H */
