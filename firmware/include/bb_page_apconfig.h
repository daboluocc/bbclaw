/**
 * APCONFIG page — full-screen dot-matrix WiFi 配网 screen.
 *
 * Replaces the old "stuff the AP SSID/PWD/IP into a chat turn" display
 * (bb_display_show_chat_turn) with a dedicated page in the same visual
 * language as bb_page_netconn: a broadcasting WiFi glyph on the left and a
 * three-step join guide (热点 / 密码 / 打开) on the right. Shown while the
 * device is in SoftAP provisioning mode; persists until the user submits
 * credentials and bb_wifi esp_restart()s, so there is no auto-dismiss path.
 *
 * All calls take the LVGL port lock internally; callers hold no lock.
 * The page lives on lv_layer_top() — synchronous hard-cut show/dismiss,
 * same NO_MEM lesson as bb_page_netconn / bb_page_boot.
 */
#ifndef BB_PAGE_APCONFIG_H
#define BB_PAGE_APCONFIG_H

#ifdef __cplusplus
extern "C" {
#endif

/** Create the page on lv_layer_top(), snapshot the AP SSID/password/IP from
 *  bb_wifi_get_ap_* and start the broadcast pulse animation. No-op if the
 *  page already exists (safe to call every loop tick in the runtime-drop
 *  provisioning path). */
void bb_page_apconfig_show(void);

/** Destroy the page synchronously (hard cut, no fade). No-op if never
 *  shown / already dismissed. */
void bb_page_apconfig_dismiss(void);

/** 1 while the page object exists. */
int bb_page_apconfig_active(void);

#ifdef __cplusplus
}
#endif

#endif /* BB_PAGE_APCONFIG_H */
