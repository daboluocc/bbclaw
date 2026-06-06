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

/** Fade the splash out (~350ms) and delete it. No-op if never shown /
 *  already dismissed. */
void bb_page_boot_dismiss(void);

/** 1 while the splash object exists (including during the fade-out). */
int bb_page_boot_active(void);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_BOOT_H */
