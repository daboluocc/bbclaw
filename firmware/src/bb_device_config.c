#include "bb_device_config.h"

#include <string.h>
#include "cJSON.h"
#include "esp_log.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_device_config";

#define BB_CONFIG_NVS_NS  "bbclaw"
#define BB_CONFIG_NVS_KEY "device/config"

static bb_device_config_t s_config = {
  .version = 0,
  .miyu_enabled = 0,
  .volume_pct = 80,
  .speed_ratio_x10 = 10,
  .speaker_enabled = 1,
};

static int s_loaded = 0;

esp_err_t bb_device_config_load(void) {
  if (s_loaded) return ESP_OK;

  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CONFIG_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) {
    ESP_LOGI(TAG, "config not in nvs (%s), using defaults", esp_err_to_name(err));
    s_loaded = 1;
    return ESP_ERR_NOT_FOUND;
  }

  size_t len = 0;
  err = nvs_get_blob(h, BB_CONFIG_NVS_KEY, NULL, &len);
  if (err != ESP_OK || len != sizeof(s_config)) {
    ESP_LOGI(TAG, "config blob invalid (%s), using defaults", esp_err_to_name(err));
    nvs_close(h);
    s_loaded = 1;
    return ESP_ERR_NOT_FOUND;
  }

  err = nvs_get_blob(h, BB_CONFIG_NVS_KEY, &s_config, &len);
  nvs_close(h);
  s_loaded = 1;

  if (err == ESP_OK) {
    ESP_LOGI(TAG, "config loaded version=%d miyu=%d volume=%d speed=%d speaker=%d",
             s_config.version, s_config.miyu_enabled, s_config.volume_pct,
             s_config.speed_ratio_x10, s_config.speaker_enabled);
    return ESP_OK;
  }

  ESP_LOGW(TAG, "config load failed (%s), using defaults", esp_err_to_name(err));
  return err;
}

const bb_device_config_t* bb_device_config_get(void) {
  return &s_config;
}

static esp_err_t persist_config(void) {
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_CONFIG_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) return err;

  err = nvs_set_blob(h, BB_CONFIG_NVS_KEY, &s_config, sizeof(s_config));
  if (err == ESP_OK) {
    err = nvs_commit(h);
  }
  nvs_close(h);

  if (err == ESP_OK) {
    ESP_LOGI(TAG, "config persisted version=%d", s_config.version);
  } else {
    ESP_LOGW(TAG, "config persist failed (%s)", esp_err_to_name(err));
  }
  return err;
}

esp_err_t bb_device_config_apply_update(int version, const char* updates_json) {
  if (updates_json == NULL || updates_json[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }

  if (version <= s_config.version) {
    ESP_LOGI(TAG, "config update ignored: version %d <= current %d", version, s_config.version);
    return ESP_OK;
  }

  cJSON* root = cJSON_Parse(updates_json);
  if (root == NULL) {
    ESP_LOGE(TAG, "config update parse failed");
    return ESP_ERR_INVALID_ARG;
  }

  int updated = 0;
  cJSON* item;

  if ((item = cJSON_GetObjectItem(root, "miyu_enabled")) != NULL && cJSON_IsBool(item)) {
    s_config.miyu_enabled = cJSON_IsTrue(item) ? 1 : 0;
    updated++;
  }
  if ((item = cJSON_GetObjectItem(root, "volume_pct")) != NULL && cJSON_IsNumber(item)) {
    s_config.volume_pct = item->valueint;
    updated++;
  }
  if ((item = cJSON_GetObjectItem(root, "speed_ratio_x10")) != NULL && cJSON_IsNumber(item)) {
    s_config.speed_ratio_x10 = item->valueint;
    updated++;
  }
  if ((item = cJSON_GetObjectItem(root, "speaker_enabled")) != NULL && cJSON_IsBool(item)) {
    s_config.speaker_enabled = cJSON_IsTrue(item) ? 1 : 0;
    updated++;
  }

  cJSON_Delete(root);

  if (updated > 0) {
    s_config.version = version;
    ESP_LOGI(TAG, "config updated version=%d fields=%d miyu=%d", version, updated, s_config.miyu_enabled);
    return persist_config();
  }

  ESP_LOGI(TAG, "config update had no recognized fields");
  return ESP_OK;
}

esp_err_t bb_device_config_apply_welcome(const char* config_json) {
  if (config_json == NULL || config_json[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }

  cJSON* root = cJSON_Parse(config_json);
  if (root == NULL) {
    ESP_LOGE(TAG, "config welcome parse failed");
    return ESP_ERR_INVALID_ARG;
  }

  cJSON* item;
  bb_device_config_t new_config = s_config;

  if ((item = cJSON_GetObjectItem(root, "version")) != NULL && cJSON_IsNumber(item)) {
    new_config.version = item->valueint;
  }
  if ((item = cJSON_GetObjectItem(root, "miyu_enabled")) != NULL && cJSON_IsBool(item)) {
    new_config.miyu_enabled = cJSON_IsTrue(item) ? 1 : 0;
  }
  if ((item = cJSON_GetObjectItem(root, "volume_pct")) != NULL && cJSON_IsNumber(item)) {
    new_config.volume_pct = item->valueint;
  }
  if ((item = cJSON_GetObjectItem(root, "speed_ratio_x10")) != NULL && cJSON_IsNumber(item)) {
    new_config.speed_ratio_x10 = item->valueint;
  }
  if ((item = cJSON_GetObjectItem(root, "speaker_enabled")) != NULL && cJSON_IsBool(item)) {
    new_config.speaker_enabled = cJSON_IsTrue(item) ? 1 : 0;
  }

  cJSON_Delete(root);

  if (new_config.version > s_config.version) {
    s_config = new_config;
    ESP_LOGI(TAG, "config from welcome version=%d miyu=%d", s_config.version, s_config.miyu_enabled);
    return persist_config();
  }

  ESP_LOGI(TAG, "config from welcome ignored: version %d <= current %d", new_config.version, s_config.version);
  return ESP_OK;
}
