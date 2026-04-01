#include "bb_wifi.h"

static const char* s_sim_ssid = "SimWiFi";

const char* bb_wifi_get_active_ssid(void) { return s_sim_ssid; }
int bb_wifi_get_rssi(void) { return -55; }
