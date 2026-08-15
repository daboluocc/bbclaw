#pragma once

#include "esp_err.h"

typedef enum {
  BB_WIFI_MODE_NONE = 0,
  BB_WIFI_MODE_STA_CONNECTED = 1,
  BB_WIFI_MODE_AP_PROVISIONING = 2,
  BB_WIFI_MODE_STA_RECONNECTING = 3, /**< 运行期掉线，指数退避自动重连中 */
} bb_wifi_mode_t;

esp_err_t bb_wifi_init_and_connect(void);
int bb_wifi_is_connected(void);
int bb_wifi_is_provisioning_mode(void);

/** 调试注入(devmon 202):运行期强制进配网门户——复现「断网重启进配网→
 *  WiFi 恢复→后台扫描重连」链路,不用真拔路由器。 */
esp_err_t bb_wifi_debug_enter_provisioning(void);
bb_wifi_mode_t bb_wifi_get_mode(void);
const char* bb_wifi_get_active_ssid(void);
const char* bb_wifi_get_ap_ssid(void);
const char* bb_wifi_get_ap_password(void);
const char* bb_wifi_get_ap_ip(void);
/** Return current STA RSSI in dBm, or 0 if not connected. */
int bb_wifi_get_rssi(void);

/** Load the SSID of a saved-WiFi slot (0..BBCLAW_WIFI_MAX_SAVED-1). Returns
 *  ESP_ERR_NOT_FOUND (or similar) if that slot is empty — callers should scan
 *  the whole range rather than assume slots are contiguous. */
esp_err_t bb_wifi_saved_get(int slot, char* ssid, size_t ssid_size);

/** Forget (delete) a saved-WiFi slot; remaining slots compact down. Does not
 *  affect an already-established connection using that slot's credentials. */
esp_err_t bb_wifi_forget_saved(int slot);
