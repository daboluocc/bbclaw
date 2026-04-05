#include "bb_identity.h"

#include "esp_app_desc.h"
#include "esp_log.h"
#include "esp_mac.h"

#include <stdio.h>
#include <string.h>

static const char *TAG = "bb_identity";

static char s_device_id[64];
static char s_session_key[96];
static int s_ready;

void bbclaw_identity_init(void) {
  const esp_app_desc_t *app = esp_app_get_description();
  const char *ver = (app && app->version[0]) ? app->version : "dev";
  uint8_t mac[6] = {0};
  if (esp_read_mac(mac, ESP_MAC_WIFI_SOFTAP) == ESP_OK) {
    snprintf(s_device_id, sizeof(s_device_id), "BBClaw-%s-%02X%02X%02X", ver, mac[3], mac[4], mac[5]);
    snprintf(s_session_key, sizeof(s_session_key), "agent:main:bbclaw-%02x%02x%02x", mac[3], mac[4], mac[5]);
  } else {
    snprintf(s_device_id, sizeof(s_device_id), "BBClaw-%s-unknown", ver);
    snprintf(s_session_key, sizeof(s_session_key), "agent:main:bbclaw-unknown");
  }
  s_ready = 1;
  ESP_LOGI(TAG, "device_id=%s", s_device_id);
  ESP_LOGI(TAG, "session_key=%s", s_session_key);
}

const char *bbclaw_device_id(void) {
  if (!s_ready) {
    bbclaw_identity_init();
  }
  return s_device_id;
}

const char *bbclaw_session_key(void) {
  if (!s_ready) {
    bbclaw_identity_init();
  }
  return s_session_key;
}
