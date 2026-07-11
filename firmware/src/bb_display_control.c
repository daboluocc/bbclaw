/**
 * 显示屏亮度控制实现 — 通用框架
 *
 * 支持 ST7789 / AMOLED (CO5300) / LCD 等多种显示芯片。
 * 具体芯片实现通过条件编译选择。
 */

#include "bb_display_control.h"
#include "bb_config.h"

#include <esp_log.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

static const char* TAG = "display_control";

/* ── 亮度控制状态 ── */
typedef struct {
  int initialized;
  uint8_t current_level;           /* 当前等级 (0-10) */
  uint8_t current_raw;             /* 当前原始值 (0-255) */
  int fading;                      /* 正在渐进中 */
  uint8_t target_level;            /* 渐进目标等级 */
  uint32_t fade_start_ms;          /* 渐进开始时间 */
  uint32_t fade_duration_ms;       /* 渐进总时长 */
  TaskHandle_t fade_task_handle;   /* 渐进任务句柄 */
  volatile uint32_t fade_gen;      /* 代数:变化=旧渐变任务自行退出(禁止外部
                                      vTaskDelete 击杀——曾在 QSPI 写一半时被杀,
                                      驱动状态烂掉→uxListRemove 随机崩) */
} display_control_state_t;

static display_control_state_t g_state = {0};

/* ── 等级到原始值的映射表 ── */
static const uint8_t g_level_to_raw[11] = {
  0x00,   /* 0:   OFF  0% */
  0x0D,   /* 1:   MIN  5% */
  0x33,   /* 2:   LOW  20% */
  0x7F,   /* 3:        50% */
  0xAC,   /* 4:        67% */
  0xCC,   /* 5:        80% */
  0xD9,   /* 6:        85% */
  0xE6,   /* 7:        90% */
  0xF2,   /* 8:   HIGH 95% */
  0xF9,   /* 9:        99% */
  0xFF,   /* 10:  MAX  100% */
};

/* ── 平台相关的亮度设置函数 ── */

/**
 * 平台相关：设置原始亮度值。由具体芯片驱动实现。
 *
 * @param value 亮度值 (0-255)
 * @return ESP_OK 成功
 */
extern esp_err_t bb_display_set_brightness_raw_impl(uint8_t value);
extern esp_err_t bb_display_set_panel_on_impl(int on);

esp_err_t bb_display_set_panel_on(int on) {
  if (!g_state.initialized) {
    return ESP_ERR_INVALID_STATE;
  }
  return bb_display_set_panel_on_impl(on);
}

/* ── 公共接口实现 ── */

esp_err_t bb_display_brightness_init(void) {
#ifndef BBCLAW_DISPLAY_BRIGHTNESS_CONTROL
  ESP_LOGI(TAG, "Display brightness control disabled by board config");
  return ESP_ERR_NOT_SUPPORTED;
#endif

  if (g_state.initialized) {
    return ESP_OK;
  }

  g_state.initialized = 1;
  g_state.current_level = BB_BRIGHTNESS_MAX;
  g_state.current_raw = g_level_to_raw[BB_BRIGHTNESS_MAX];
  g_state.fading = 0;
  g_state.fade_task_handle = NULL;

  /* 调用平台相关的初始化（如 AMOLED 命令设置） */
  esp_err_t ret = bb_display_set_brightness_raw_impl(g_state.current_raw);
  if (ret == ESP_ERR_NOT_SUPPORTED) {
    ESP_LOGW(TAG, "Platform does not support brightness control");
    return ESP_ERR_NOT_SUPPORTED;
  }

  ESP_LOGI(TAG, "Display brightness control initialized (level=%d, raw=0x%02x)",
           g_state.current_level, g_state.current_raw);

  return ESP_OK;
}

esp_err_t bb_display_brightness_deinit(void) {
  if (!g_state.initialized) {
    return ESP_OK;
  }

  if (g_state.fading) {
    (void)bb_display_stop_fade();
  }

  g_state.initialized = 0;
  ESP_LOGI(TAG, "Display brightness control deinitialized");
  return ESP_OK;
}

int bb_display_brightness_is_available(void) {
  return g_state.initialized;
}

esp_err_t bb_display_set_brightness_level(uint8_t level) {
  if (level > BB_BRIGHTNESS_MAX) {
    return ESP_ERR_INVALID_ARG;
  }

  if (!g_state.initialized) {
    return ESP_ERR_INVALID_STATE;
  }

  /* 停止任何正在进行的渐进 */
  if (g_state.fading) {
    bb_display_stop_fade();
  }

  g_state.current_level = level;
  g_state.current_raw = g_level_to_raw[level];

  esp_err_t ret = bb_display_set_brightness_raw_impl(g_state.current_raw);
  if (ret == ESP_OK) {
    ESP_LOGD(TAG, "Brightness set to level %d (raw=0x%02x)", level, g_state.current_raw);
  }

  return ret;
}

uint8_t bb_display_get_brightness_level(void) {
  return g_state.current_level;
}

esp_err_t bb_display_set_brightness_raw(uint8_t value) {
  if (!g_state.initialized) {
    return ESP_ERR_INVALID_STATE;
  }

  if (g_state.fading) {
    bb_display_stop_fade();
  }

  g_state.current_raw = value;
  /* 反推等级（简单查表） */
  g_state.current_level = 10;
  for (uint8_t i = 0; i < 11; i++) {
    if (g_level_to_raw[i] >= value) {
      g_state.current_level = i;
      break;
    }
  }

  esp_err_t ret = bb_display_set_brightness_raw_impl(value);
  if (ret == ESP_OK) {
    ESP_LOGD(TAG, "Brightness set to raw 0x%02x (level≈%d)", value, g_state.current_level);
  }

  return ret;
}

uint8_t bb_display_get_brightness_raw(void) {
  return g_state.current_raw;
}

/* ── 渐进淡出任务 ── */

static void fade_task(void* arg) {
  const uint32_t my_gen = (uint32_t)(uintptr_t)arg;
  while (g_state.fading && g_state.fade_gen == my_gen) {
    uint32_t now = xTaskGetTickCount() * portTICK_PERIOD_MS;
    uint32_t elapsed = now - g_state.fade_start_ms;

    if (elapsed >= g_state.fade_duration_ms) {
      /* 渐进完成，设置目标亮度 */
      uint8_t final_level = g_state.target_level;
      g_state.fading = 0;
      g_state.fade_task_handle = NULL;
      bb_display_set_brightness_level(final_level);
      vTaskDelete(NULL);
    }

    /* 线性插值 */
    uint8_t start_raw = g_state.current_raw;
    uint8_t target_raw = g_level_to_raw[g_state.target_level];
    uint16_t intermediate;

    if (target_raw >= start_raw) {
      intermediate = start_raw + (uint16_t)(target_raw - start_raw) * elapsed / g_state.fade_duration_ms;
    } else {
      intermediate = start_raw - (uint16_t)(start_raw - target_raw) * elapsed / g_state.fade_duration_ms;
    }

    uint8_t intermediate_raw = (uint8_t)(intermediate & 0xFF);
    bb_display_set_brightness_raw_impl(intermediate_raw);

    vTaskDelay(pdMS_TO_TICKS(50));  /* 20Hz 更新频率 */
  }
  /* 代数变化/被 stop:自行退出 */
  vTaskDelete(NULL);
}

esp_err_t bb_display_fade_brightness(uint8_t target_level, uint16_t duration_ms) {
  if (target_level > BB_BRIGHTNESS_MAX) {
    return ESP_ERR_INVALID_ARG;
  }

  if (!g_state.initialized) {
    return ESP_ERR_INVALID_STATE;
  }

  /* 如果已在渐进中且目标相同，直接返回 */
  if (g_state.fading && g_state.target_level == target_level) {
    return ESP_OK;
  }

  /* 停止当前渐进 */
  if (g_state.fading) {
    bb_display_stop_fade();
  }

  /* 如果时长为 0，立即切换 */
  if (duration_ms == 0) {
    return bb_display_set_brightness_level(target_level);
  }

  g_state.target_level = target_level;
  g_state.fade_duration_ms = duration_ms;
  g_state.fade_start_ms = xTaskGetTickCount() * portTICK_PERIOD_MS;
  g_state.fading = 1;
  g_state.fade_gen++;

  BaseType_t ret = xTaskCreate(fade_task, "brightness_fade", 2048, (void*)(uintptr_t)g_state.fade_gen, 4,
                               &g_state.fade_task_handle);
  if (ret != pdPASS) {
    g_state.fading = 0;
    return ESP_ERR_NO_MEM;
  }

  ESP_LOGD(TAG, "Fading brightness from level %d to %d over %dms",
           g_state.current_level, target_level, duration_ms);

  return ESP_OK;
}

esp_err_t bb_display_stop_fade(void) {
  if (!g_state.fading) {
    return ESP_OK;
  }
  /* 优雅停:置代数+清 fading,任务在下一个 50ms 周期自行退出。
   * 绝不 vTaskDelete 一个可能正在做 QSPI 写的任务。 */
  g_state.fade_gen++;
  g_state.fading = 0;
  g_state.fade_task_handle = NULL;
  ESP_LOGD(TAG, "Brightness fade stop requested");
  return ESP_OK;
}

int bb_display_is_fading(void) {
  return g_state.fading;
}
