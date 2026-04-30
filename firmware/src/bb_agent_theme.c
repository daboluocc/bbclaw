#include "bb_agent_theme.h"

#include <string.h>

#include "esp_err.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char* TAG = "bb_agent_theme";

#define BB_THEME_NVS_NS   "bbclaw"
#define BB_THEME_NVS_KEY  "agent/theme"
/* Phase 4.7.2: default to buddy-anim (animated ASCII character theme).
 * Theme selection removed from Settings — buddy-anim is the only theme. */
#define BB_THEME_FALLBACK "buddy-anim"
#define BB_THEME_NAME_MAX 24
#define BB_THEME_CAP      8

/*
 * 主题注册表：cap 8 的小静态数组。设计上主题对象（bb_agent_theme_t）是 static const，
 * 注册时只存指针，不复制内容。线程安全用一个 mutex 保护读写（注册可能在 constructor /
 * 启动期，get_active 可能在 LVGL 任务里调用）。
 */
typedef struct {
  const bb_agent_theme_t* themes[BB_THEME_CAP];
  const char* names[BB_THEME_CAP]; /* 镜像 themes[i]->name，给 _list 用，避免暴露内部对象 */
  int count;
  const bb_agent_theme_t* active;
  int active_loaded; /* NVS 懒加载缓存标记 */
  SemaphoreHandle_t lock;
  StaticSemaphore_t lock_buf;
} bb_theme_registry_t;

static bb_theme_registry_t s_reg = {0};

static void registry_lock_init_once(void) {
  /* 首次访问时把 mutex 建好；constructor 调用 register 时 lock 还没建，要先建。
   * 这函数本身不是线程安全的，但 constructor 在单线程启动期被调用，OK。 */
  if (s_reg.lock == NULL) {
    s_reg.lock = xSemaphoreCreateMutexStatic(&s_reg.lock_buf);
  }
}

static int find_index_locked(const char* name) {
  if (name == NULL) return -1;
  for (int i = 0; i < s_reg.count; i++) {
    if (s_reg.themes[i] != NULL && s_reg.themes[i]->name != NULL &&
        strcmp(s_reg.themes[i]->name, name) == 0) {
      return i;
    }
  }
  return -1;
}

void bb_agent_theme_register(const bb_agent_theme_t* theme) {
  if (theme == NULL || theme->name == NULL) {
    ESP_LOGW(TAG, "register: NULL theme/name");
    return;
  }
  registry_lock_init_once();
  if (xSemaphoreTake(s_reg.lock, portMAX_DELAY) != pdTRUE) {
    return;
  }
  int existing = find_index_locked(theme->name);
  if (existing >= 0) {
    ESP_LOGW(TAG, "register: theme '%s' already exists, overwriting", theme->name);
    s_reg.themes[existing] = theme;
    s_reg.names[existing] = theme->name;
    /* 如果当前激活的就是这个，刷新指针 */
    if (s_reg.active != NULL && strcmp(s_reg.active->name, theme->name) == 0) {
      s_reg.active = theme;
    }
    xSemaphoreGive(s_reg.lock);
    return;
  }
  if (s_reg.count >= BB_THEME_CAP) {
    ESP_LOGE(TAG, "register: registry full (cap=%d), dropping '%s'", BB_THEME_CAP, theme->name);
    xSemaphoreGive(s_reg.lock);
    return;
  }
  s_reg.themes[s_reg.count] = theme;
  s_reg.names[s_reg.count] = theme->name;
  s_reg.count++;
  ESP_LOGI(TAG, "register: '%s' (total=%d)", theme->name, s_reg.count);
  xSemaphoreGive(s_reg.lock);
}

/*
 * 从 NVS 读 agent/theme；找不到 / namespace 不存在 / 主题名未注册 → fallback "text-only"。
 * 必须在持锁状态下调用（写 s_reg.active）。
 */
static void load_active_from_nvs_locked(void) {
  char buf[BB_THEME_NAME_MAX] = {0};
  size_t sz = sizeof(buf);
  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_THEME_NVS_NS, NVS_READONLY, &h);
  if (err == ESP_OK) {
    err = nvs_get_str(h, BB_THEME_NVS_KEY, buf, &sz);
    nvs_close(h);
  }

  const bb_agent_theme_t* picked = NULL;
  if (err == ESP_OK && buf[0] != '\0') {
    int idx = find_index_locked(buf);
    if (idx >= 0) {
      picked = s_reg.themes[idx];
      ESP_LOGI(TAG, "active loaded from nvs: '%s'", buf);
    } else {
      ESP_LOGW(TAG, "nvs theme '%s' not registered, falling back", buf);
    }
  } else if (err != ESP_OK) {
    ESP_LOGI(TAG, "nvs no theme set (%s), falling back", esp_err_to_name(err));
  }

  if (picked == NULL) {
    int idx = find_index_locked(BB_THEME_FALLBACK);
    if (idx >= 0) {
      picked = s_reg.themes[idx];
      ESP_LOGI(TAG, "active = fallback '%s'", BB_THEME_FALLBACK);
    } else if (s_reg.count > 0) {
      /* 连 fallback 都没注册——拿第一个，至少有东西用。 */
      picked = s_reg.themes[0];
      ESP_LOGW(TAG, "fallback '%s' missing, using '%s'", BB_THEME_FALLBACK,
               picked->name != NULL ? picked->name : "(nameless)");
    }
  }

  s_reg.active = picked;
  s_reg.active_loaded = 1;
}

const bb_agent_theme_t* bb_agent_theme_get_active(void) {
  registry_lock_init_once();
  if (xSemaphoreTake(s_reg.lock, portMAX_DELAY) != pdTRUE) {
    return NULL;
  }
  if (!s_reg.active_loaded) {
    load_active_from_nvs_locked();
  }
  const bb_agent_theme_t* out = s_reg.active;
  xSemaphoreGive(s_reg.lock);
  return out;
}

esp_err_t bb_agent_theme_set_active(const char* name) {
  if (name == NULL || name[0] == '\0') {
    return ESP_ERR_INVALID_ARG;
  }
  if (strlen(name) >= BB_THEME_NAME_MAX) {
    return ESP_ERR_INVALID_ARG;
  }
  registry_lock_init_once();
  if (xSemaphoreTake(s_reg.lock, portMAX_DELAY) != pdTRUE) {
    return ESP_FAIL;
  }
  int idx = find_index_locked(name);
  if (idx < 0) {
    ESP_LOGW(TAG, "set_active: theme '%s' not registered", name);
    xSemaphoreGive(s_reg.lock);
    return ESP_ERR_NOT_FOUND;
  }

  nvs_handle_t h;
  esp_err_t err = nvs_open(BB_THEME_NVS_NS, NVS_READWRITE, &h);
  if (err == ESP_OK) {
    err = nvs_set_str(h, BB_THEME_NVS_KEY, name);
    if (err == ESP_OK) {
      err = nvs_commit(h);
    }
    nvs_close(h);
  }
  if (err != ESP_OK) {
    ESP_LOGE(TAG, "set_active: nvs write failed: %s", esp_err_to_name(err));
    xSemaphoreGive(s_reg.lock);
    return err;
  }

  s_reg.active = s_reg.themes[idx];
  s_reg.active_loaded = 1;
  ESP_LOGI(TAG, "set_active: '%s'", name);
  xSemaphoreGive(s_reg.lock);
  return ESP_OK;
}

const char* const* bb_agent_theme_list(int* out_count) {
  registry_lock_init_once();
  if (xSemaphoreTake(s_reg.lock, portMAX_DELAY) != pdTRUE) {
    if (out_count != NULL) *out_count = 0;
    return NULL;
  }
  if (out_count != NULL) {
    *out_count = s_reg.count;
  }
  /* names[] 在注册期由 register 写入；list 后续不再变（除非 register 又被调）。
   * 因为 themes/name 都是 static const、生命期等同程序，返回裸指针 OK。 */
  const char* const* out = (const char* const*)s_reg.names;
  xSemaphoreGive(s_reg.lock);
  return out;
}
