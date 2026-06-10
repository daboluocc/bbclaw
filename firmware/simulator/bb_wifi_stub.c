#include "bb_wifi.h"

static const char* s_sim_ssid = "SimWiFi";

const char* bb_wifi_get_active_ssid(void) { return s_sim_ssid; }
int bb_wifi_get_rssi(void) { return -55; }
/* Stay "connecting" so the netconn preview never self-dismisses. */
int bb_wifi_is_connected(void) { return 0; }

/* AP provisioning getters — sample values for the apconfig page preview. */
const char* bb_wifi_get_ap_ssid(void) { return "BBClaw-C7EB88"; }
const char* bb_wifi_get_ap_password(void) { return "12345678"; }
const char* bb_wifi_get_ap_ip(void) { return "192.168.4.1"; }
