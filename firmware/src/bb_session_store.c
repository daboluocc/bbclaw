#include "bb_session_store.h"

#include <string.h>
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_session_store";

#define BB_SESSION_NVS_NS "bbclaw"
#define BB_SESSION_PERSIST_TASK_STACK 4096
#define BB_SESSION_PERSIST_TASK_PRIO 3
#define BB_SESSION_MIGRATE_FLAG "ls_migrated"

/* Driver name → NVS key short prefix mapping (ADR-014: ls/ prefix) */
typedef struct {
  const char* driver_name;
  const char* nvs_key;
  const char* legacy_key;  /* v0.4.x key for migration cleanup */
} driver_key_map_t;

static const driver_key_map_t s_driver_map[] = {
  {"claude-code", "ls/cc", "s/cc"},
  {"opencode",    "ls/oc", "s/oc"},
  {"openclaw",    "ls/op", "s/op"},
  {"ollama",      "ls/ol", "s/ol"},
};

static const char* driver_to_nvs_key(const char* driver_name) {
  if (driver_name == NULL) return NULL;
  for (size_t i = 0; i < sizeof(s_driver_map) / sizeof(s_driver_map[0]); ++i) {
    if (strcmp(driver_name, s_driver_map[i].driver_name) == 0) {
      return s_driver_map[i].nvs_key;
    }
  }
  return NULL;
}

void bb_session_store_migrate(void) {
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SESSION_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "migrate: nvs_open failed (%s)", esp_err_to_name(err));
    return;
  }

  /* Check if migration already ran */
  uint8_t migrated = 0;
  err = nvs_get_u8(h, BB_SESSION_MIGRATE_FLAG, &migrated);
  if (err == ESP_OK && migrated == 1) {
    nvs_close(h);
    ESP_LOGD(TAG, "migrate: already done, skipping");
    return;
  }

  /* Erase all legacy "s/xx" keys (don't migrate values -- old CLI session IDs
   * are likely invalid after upgrade per ADR-014) */
  int erased = 0;
  for (size_t i = 0; i < sizeof(s_driver_map) / sizeof(s_driver_map[0]); ++i) {
    err = nvs_erase_key(h, s_driver_map[i].legacy_key);
    if (err == ESP_OK) {
      ESP_LOGI(TAG, "migrate: erased legacy key '%s'", s_driver_map[i].legacy_key);
      erased++;
    }
    /* ESP_ERR_NVS_NOT_FOUND is fine — key didn't exist */
  }

  /* Set migration flag so we don't repeat on next boot */
  nvs_set_u8(h, BB_SESSION_MIGRATE_FLAG, 1);
  nvs_commit(h);
  nvs_close(h);

  ESP_LOGI(TAG, "migrate: complete, erased %d legacy keys", erased);
}

esp_err_t bb_session_store_load(const char* driver_name, char* out_sid, size_t sz) {
  if (driver_name == NULL || out_sid == NULL || sz == 0) {
    return ESP_ERR_INVALID_ARG;
  }
  
  const char* key = driver_to_nvs_key(driver_name);
  if (key == NULL) {
    ESP_LOGW(TAG, "load: unknown driver '%s'", driver_name);
    return ESP_ERR_INVALID_ARG;
  }
  
  out_sid[0] = '\0';
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SESSION_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) {
    return err;
  }
  
  err = nvs_get_str(h, key, out_sid, &sz);
  nvs_close(h);
  
  if (err == ESP_OK) {
    ESP_LOGI(TAG, "load: driver='%s' sid='%s'", driver_name, out_sid);
  } else if (err == ESP_ERR_NVS_NOT_FOUND) {
    ESP_LOGD(TAG, "load: driver='%s' no stored session", driver_name);
  }
  
  return err;
}

/* Deferred persist task payload */
typedef struct {
  char driver_name[24];
  char session_id[64];
} persist_payload_t;

static void persist_task(void* arg) {
  persist_payload_t* p = (persist_payload_t*)arg;
  if (p == NULL) {
    vTaskDelete(NULL);
    return;
  }
  
  const char* key = driver_to_nvs_key(p->driver_name);
  if (key == NULL) {
    ESP_LOGW(TAG, "persist_task: unknown driver '%s'", p->driver_name);
    free(p);
    vTaskDelete(NULL);
    return;
  }
  
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SESSION_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "persist_task: nvs_open failed (%s)", esp_err_to_name(err));
    free(p);
    vTaskDelete(NULL);
    return;
  }
  
  if (p->session_id[0] == '\0') {
    /* Empty session ID → erase the key */
    err = nvs_erase_key(h, key);
    if (err == ESP_OK || err == ESP_ERR_NVS_NOT_FOUND) {
      err = nvs_commit(h);
      ESP_LOGI(TAG, "persist_task: driver='%s' session cleared", p->driver_name);
    }
  } else {
    err = nvs_set_str(h, key, p->session_id);
    if (err == ESP_OK) {
      err = nvs_commit(h);
      ESP_LOGI(TAG, "persist_task: driver='%s' sid='%s'", p->driver_name, p->session_id);
    }
  }
  
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "persist_task: write failed (%s)", esp_err_to_name(err));
  }
  
  nvs_close(h);
  free(p);
  vTaskDelete(NULL);
}

esp_err_t bb_session_store_save(const char* driver_name, const char* session_id) {
  if (driver_name == NULL || session_id == NULL) {
    return ESP_ERR_INVALID_ARG;
  }
  
  const char* key = driver_to_nvs_key(driver_name);
  if (key == NULL) {
    ESP_LOGW(TAG, "save: unknown driver '%s'", driver_name);
    return ESP_ERR_INVALID_ARG;
  }
  
  persist_payload_t* p = (persist_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) {
    return ESP_ERR_NO_MEM;
  }
  
  strncpy(p->driver_name, driver_name, sizeof(p->driver_name) - 1);
  strncpy(p->session_id, session_id, sizeof(p->session_id) - 1);
  
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(persist_task, "session_persist",
                              BB_SESSION_PERSIST_TASK_STACK, p,
                              BB_SESSION_PERSIST_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "save: xTaskCreate failed");
    free(p);
    return ESP_FAIL;
  }
  
  return ESP_OK;
}
