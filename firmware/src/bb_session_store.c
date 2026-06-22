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

/* Active driver name (this ADR). Single string key under the bbclaw
 * namespace; max 23 chars + NUL covers every current driver id. */
#define BB_SESSION_NVS_KEY_ACTIVE_DRIVER "drv/active"

/* Active adapter home_site_id (the SaaS adapter the user last selected). A
 * UUID (36 chars) + NUL. Persisted so Settings can show the last adapter
 * immediately without waiting on a sites.list fetch that may fail right after
 * boot; routing itself is server-side. */
#define BB_SESSION_NVS_KEY_ACTIVE_SITE "site/active"
#define BB_ACTIVE_SITE_CACHE_SZ 40

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
  /* ADR-016: aider added — was previously not selectable on device since
   * Settings is the only entry point and Settings only landed with ADR-016. */
  {"aider",       "ls/ai", "s/ai"},
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

/* ── Active driver name (this ADR) ──
 *
 * NVS read of "drv/active" is done ONCE from app_main via
 * bb_session_store_preload_nvs() and cached in s_active_driver_cache so
 * later reads from PSRAM-stack tasks (stream_task etc.) can hit memory
 * directly. Issuing nvs_get_str from a PSRAM-stack task panics with
 * esp_task_stack_is_sane_cache_disabled() because NVS temporarily disables
 * SPI flash cache, and PSRAM stacks become unreachable without it. */

#define BB_ACTIVE_DRIVER_CACHE_SZ 24

static char s_active_driver_cache[BB_ACTIVE_DRIVER_CACHE_SZ] = {0};
static int  s_active_driver_loaded = 0;

static char s_active_site_cache[BB_ACTIVE_SITE_CACHE_SZ] = {0};

void bb_session_store_preload_nvs(void) {
  if (s_active_driver_loaded) return;
  s_active_driver_loaded = 1;
  s_active_driver_cache[0] = '\0';
  s_active_site_cache[0] = '\0';
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SESSION_NVS_NS, NVS_READONLY, &h);
  if (err != ESP_OK) {
    ESP_LOGD(TAG, "preload_nvs: nvs_open failed (%s) — cache stays empty",
             esp_err_to_name(err));
    return;
  }
  size_t sz = sizeof(s_active_driver_cache);
  err = nvs_get_str(h, BB_SESSION_NVS_KEY_ACTIVE_DRIVER, s_active_driver_cache, &sz);
  if (err == ESP_OK) {
    ESP_LOGI(TAG, "preload_nvs: active_driver='%s'", s_active_driver_cache);
  } else if (err == ESP_ERR_NVS_NOT_FOUND) {
    ESP_LOGD(TAG, "preload_nvs: active_driver not set yet");
  } else {
    ESP_LOGW(TAG, "preload_nvs: active_driver read failed (%s)", esp_err_to_name(err));
    s_active_driver_cache[0] = '\0';
  }
  size_t ssz = sizeof(s_active_site_cache);
  err = nvs_get_str(h, BB_SESSION_NVS_KEY_ACTIVE_SITE, s_active_site_cache, &ssz);
  if (err == ESP_OK) {
    ESP_LOGI(TAG, "preload_nvs: active_site='%s'", s_active_site_cache);
  } else if (err != ESP_ERR_NVS_NOT_FOUND) {
    s_active_site_cache[0] = '\0';
  }
  nvs_close(h);
}

esp_err_t bb_session_store_load_active_site(char* out_id, size_t sz) {
  if (out_id == NULL || sz == 0) return ESP_ERR_INVALID_ARG;
  out_id[0] = '\0';
  if (!s_active_driver_loaded) {
    bb_session_store_preload_nvs();
  }
  if (s_active_site_cache[0] == '\0') {
    return ESP_ERR_NVS_NOT_FOUND;
  }
  strncpy(out_id, s_active_site_cache, sz - 1);
  out_id[sz - 1] = '\0';
  return ESP_OK;
}

typedef struct {
  char site_id[BB_ACTIVE_SITE_CACHE_SZ];
} active_site_payload_t;

static void active_site_persist_task(void* arg) {
  active_site_payload_t* p = (active_site_payload_t*)arg;
  if (p == NULL) {
    vTaskDelete(NULL);
    return;
  }
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SESSION_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "active_site persist: nvs_open failed (%s)", esp_err_to_name(err));
    free(p);
    vTaskDelete(NULL);
    return;
  }
  if (p->site_id[0] == '\0') {
    err = nvs_erase_key(h, BB_SESSION_NVS_KEY_ACTIVE_SITE);
    if (err == ESP_OK || err == ESP_ERR_NVS_NOT_FOUND) err = nvs_commit(h);
  } else {
    err = nvs_set_str(h, BB_SESSION_NVS_KEY_ACTIVE_SITE, p->site_id);
    if (err == ESP_OK) {
      err = nvs_commit(h);
      ESP_LOGI(TAG, "active_site persist: '%s'", p->site_id);
    }
  }
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "active_site persist: write failed (%s)", esp_err_to_name(err));
  }
  nvs_close(h);
  free(p);
  vTaskDelete(NULL);
}

esp_err_t bb_session_store_save_active_site(const char* site_id) {
  if (site_id == NULL) return ESP_ERR_INVALID_ARG;
  s_active_driver_loaded = 1;  /* cache is now authoritative */
  strncpy(s_active_site_cache, site_id, sizeof(s_active_site_cache) - 1);
  s_active_site_cache[sizeof(s_active_site_cache) - 1] = '\0';

  active_site_payload_t* p = (active_site_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) return ESP_ERR_NO_MEM;
  strncpy(p->site_id, site_id, sizeof(p->site_id) - 1);
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(active_site_persist_task, "site_persist",
                              BB_SESSION_PERSIST_TASK_STACK, p,
                              BB_SESSION_PERSIST_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "save_active_site: xTaskCreate failed");
    free(p);
    return ESP_FAIL;
  }
  return ESP_OK;
}

esp_err_t bb_session_store_load_active_driver(char* out_name, size_t sz) {
  if (out_name == NULL || sz == 0) return ESP_ERR_INVALID_ARG;
  out_name[0] = '\0';
  /* Lazy-load: tolerate callers that didn't go through preload (e.g. tests).
   * Real device should always have preloaded by the time anyone calls in. */
  if (!s_active_driver_loaded) {
    bb_session_store_preload_nvs();
  }
  if (s_active_driver_cache[0] == '\0') {
    return ESP_ERR_NVS_NOT_FOUND;
  }
  strncpy(out_name, s_active_driver_cache, sz - 1);
  out_name[sz - 1] = '\0';
  return ESP_OK;
}

/* Deferred persist payload for the active driver key. Distinct from
 * persist_payload_t so the existing per-driver session writer stays
 * unchanged and we can extend the active-driver writer independently. */
typedef struct {
  char driver_name[24];
} active_driver_payload_t;

static void active_driver_persist_task(void* arg) {
  active_driver_payload_t* p = (active_driver_payload_t*)arg;
  if (p == NULL) {
    vTaskDelete(NULL);
    return;
  }
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_SESSION_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "active_driver persist: nvs_open failed (%s)", esp_err_to_name(err));
    free(p);
    vTaskDelete(NULL);
    return;
  }
  if (p->driver_name[0] == '\0') {
    err = nvs_erase_key(h, BB_SESSION_NVS_KEY_ACTIVE_DRIVER);
    if (err == ESP_OK || err == ESP_ERR_NVS_NOT_FOUND) {
      err = nvs_commit(h);
      ESP_LOGI(TAG, "active_driver persist: cleared");
    }
  } else {
    err = nvs_set_str(h, BB_SESSION_NVS_KEY_ACTIVE_DRIVER, p->driver_name);
    if (err == ESP_OK) {
      err = nvs_commit(h);
      ESP_LOGI(TAG, "active_driver persist: '%s'", p->driver_name);
    }
  }
  if (err != ESP_OK) {
    ESP_LOGW(TAG, "active_driver persist: write failed (%s)", esp_err_to_name(err));
  }
  nvs_close(h);
  free(p);
  vTaskDelete(NULL);
}

esp_err_t bb_session_store_save_active_driver(const char* driver_name) {
  if (driver_name == NULL) return ESP_ERR_INVALID_ARG;
  /* Refresh the in-memory cache synchronously so subsequent
   * load_active_driver calls see the new value immediately; the actual
   * NVS write still happens on the background task so we don't issue
   * flash IO from the caller's (likely PSRAM) stack. */
  s_active_driver_loaded = 1;
  strncpy(s_active_driver_cache, driver_name, sizeof(s_active_driver_cache) - 1);
  s_active_driver_cache[sizeof(s_active_driver_cache) - 1] = '\0';

  active_driver_payload_t* p = (active_driver_payload_t*)calloc(1, sizeof(*p));
  if (p == NULL) return ESP_ERR_NO_MEM;
  strncpy(p->driver_name, driver_name, sizeof(p->driver_name) - 1);
  TaskHandle_t t = NULL;
  BaseType_t ok = xTaskCreate(active_driver_persist_task, "drv_persist",
                              BB_SESSION_PERSIST_TASK_STACK, p,
                              BB_SESSION_PERSIST_TASK_PRIO, &t);
  if (ok != pdPASS) {
    ESP_LOGE(TAG, "save_active_driver: xTaskCreate failed");
    free(p);
    return ESP_FAIL;
  }
  return ESP_OK;
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
