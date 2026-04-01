#pragma once

#include "esp_err.h"

typedef enum {
  BB_WIFI_MODE_NONE = 0,
  BB_WIFI_MODE_STA_CONNECTED = 1,
  BB_WIFI_MODE_AP_PROVISIONING = 2,
} bb_wifi_mode_t;

esp_err_t bb_wifi_init_and_connect(void);
int bb_wifi_is_connected(void);
int bb_wifi_is_provisioning_mode(void);
bb_wifi_mode_t bb_wifi_get_mode(void);
const char* bb_wifi_get_active_ssid(void);
const char* bb_wifi_get_ap_ssid(void);
const char* bb_wifi_get_ap_password(void);
const char* bb_wifi_get_ap_ip(void);
/** Return current STA RSSI in dBm, or 0 if not connected. */
int bb_wifi_get_rssi(void);
