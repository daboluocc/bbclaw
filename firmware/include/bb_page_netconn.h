/**
 * NETCONN page — dot-matrix WiFi arcs + the SSID currently being tried.
 * Bridges the gap between the boot splash and a standby clock that has
 * nothing to show until SNTP syncs. See design/STATE_MACHINE.md §3.5.1.
 *
 * All calls take the LVGL port lock internally; callers hold no lock.
 */
#ifndef BB_PAGE_NETCONN_H
#define BB_PAGE_NETCONN_H

#ifdef __cplusplus
extern "C" {
#endif

/** Create the page on lv_layer_top() and start the arc animation + the
 *  wifi-state poll timer. Call right before bb_wifi_init_and_connect().
 *  The page self-dismisses once wifi is connected AND wall time is ready
 *  (or BBCLAW_NETCONN_SYNC_TIMEOUT_MS after connect). No-op if shown. */
void bb_page_netconn_show(void);

/** Destroy the page synchronously (hard cut, no fade — same NO_MEM lesson
 *  as bb_page_boot_dismiss). Call explicitly on the provisioning / wifi
 *  failure paths, which the self-dismiss conditions never reach.
 *  No-op if never shown / already dismissed. */
void bb_page_netconn_dismiss(void);

/** 1 while the page object exists. */
int bb_page_netconn_active(void);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_NETCONN_H */
