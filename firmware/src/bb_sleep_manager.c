/**
 * 息屏管理器实现 — 状态机 + IMU + 屏幕亮度联动
 *
 * 根据用户交互和 IMU 运动自动管理屏幕亮度，降低功耗。
 */

#include "bb_sleep_manager.h"
#include "bb_imu.h"
#include "bb_display_control.h"
#include "bb_board_config.h"

#include <esp_log.h>
#include <freertos/FreeRTOS.h>
#include <freertos/timers.h>
#include <math.h>

static const char* TAG = "sleep_manager";

/* ── 状态机 ── */
typedef struct {
  int initialized;
  int enabled;
  bb_sleep_state_t state;
  uint32_t last_activity_ms;
  uint32_t dimming_timeout_ms;   /* ACTIVE → DIMMING */
  uint32_t sleep_timeout_ms;     /* ACTIVE → SLEEPING (总时长) */
  uint32_t wake_cooldown_ms;     /* SLEEPING → WAKING 去抖延迟 */
  int imu_wake_enabled;
  int message_wake_enabled;
  TimerHandle_t timeout_timer;
  uint32_t wake_detected_ms;
} sleep_manager_state_t;

static sleep_manager_state_t g_state = {0};

/* ── 默认配置值 ── */
#define DEFAULT_DIMMING_TIMEOUT_MS   (2 * 60 * 1000)   /* 2 min */
#define DEFAULT_SLEEP_TIMEOUT_MS     (3 * 60 * 1000)   /* 3 min */
#define DEFAULT_WAKE_COOLDOWN_MS     2000
#define DEFAULT_IMU_ACCEL_THRESHOLD  1500               /* mg */

/* ── 状态转换 ── */

static void transition_to_state(bb_sleep_state_t new_state) {
  if (g_state.state == new_state) {
    return;
  }

  ESP_LOGI(TAG, "State transition: %d → %d", g_state.state, new_state);

  switch (new_state) {
    case BB_SLEEP_STATE_ACTIVE:
      bb_display_fade_brightness(BB_BRIGHTNESS_MAX, 200);
      break;

    case BB_SLEEP_STATE_DIMMING:
      bb_display_fade_brightness(BB_BRIGHTNESS_LOW, 500);
      break;

    case BB_SLEEP_STATE_SLEEPING:
      bb_display_fade_brightness(BB_BRIGHTNESS_OFF, 1000);
      /* 启用 IMU 低功耗模式 */
      if (g_state.imu_wake_enabled) {
        bb_imu_enable_low_power();
      }
      break;

    case BB_SLEEP_STATE_WAKING:
      /* 记录唤醒时间 */
      g_state.wake_detected_ms = xTaskGetTickCount() * portTICK_PERIOD_MS;
      bb_display_fade_brightness(BB_BRIGHTNESS_MID, 300);
      break;
  }

  g_state.state = new_state;
}

/* ── 定时器回调 ── */

static void timeout_timer_cb(TimerHandle_t timer) {
  if (!g_state.enabled) {
    return;
  }

  uint32_t now = xTaskGetTickCount() * portTICK_PERIOD_MS;
  uint32_t idle_time = now - g_state.last_activity_ms;

  switch (g_state.state) {
    case BB_SLEEP_STATE_ACTIVE:
      if (idle_time >= g_state.dimming_timeout_ms) {
        transition_to_state(BB_SLEEP_STATE_DIMMING);
      }
      break;

    case BB_SLEEP_STATE_DIMMING:
      if (idle_time >= g_state.sleep_timeout_ms) {
        transition_to_state(BB_SLEEP_STATE_SLEEPING);
      }
      break;

    case BB_SLEEP_STATE_WAKING:
      /* 检查唤醒后是否有用户交互 */
      if (idle_time > (g_state.wake_detected_ms + g_state.wake_cooldown_ms)) {
        /* 去抖延迟后仍无交互，回到睡眠 */
        transition_to_state(BB_SLEEP_STATE_SLEEPING);
      }
      break;

    case BB_SLEEP_STATE_SLEEPING:
      /* 空闲中，仅等待 IMU 唤醒或用户交互 */
      break;
  }
}

/* ── IMU 回调 ── */

static void imu_sample_cb(const bb_imu_sample_t* sample, void* arg) {
  if (!g_state.enabled || !g_state.imu_wake_enabled) {
    return;
  }

  if (g_state.state != BB_SLEEP_STATE_SLEEPING && g_state.state != BB_SLEEP_STATE_WAKING) {
    return;
  }

  /* 计算加速度总量 */
  float a_total = sqrtf(sample->accel.x * sample->accel.x +
                        sample->accel.y * sample->accel.y +
                        sample->accel.z * sample->accel.z);

  /* 简单阈值检测 */
  if (a_total > DEFAULT_IMU_ACCEL_THRESHOLD) {
    ESP_LOGI(TAG, "IMU motion detected (a_total=%.0f mg)", a_total);
    if (g_state.state == BB_SLEEP_STATE_SLEEPING) {
      transition_to_state(BB_SLEEP_STATE_WAKING);
    }
  }
}

/* ── 公共接口实现 ── */

esp_err_t bb_sleep_manager_init(void) {
#ifndef BBCLAW_DISPLAY_BRIGHTNESS_CONTROL
  ESP_LOGI(TAG, "Sleep manager disabled: display brightness control not available");
  return ESP_ERR_NOT_SUPPORTED;
#endif

  if (g_state.initialized) {
    return ESP_OK;
  }

  /* 检查依赖 */
  if (!bb_display_brightness_is_available()) {
    ESP_LOGE(TAG, "Display brightness control must be initialized first");
    return ESP_ERR_INVALID_STATE;
  }

  g_state.initialized = 1;
  g_state.enabled = 1;
  g_state.state = BB_SLEEP_STATE_ACTIVE;
  g_state.last_activity_ms = xTaskGetTickCount() * portTICK_PERIOD_MS;
  g_state.dimming_timeout_ms = DEFAULT_DIMMING_TIMEOUT_MS;
  g_state.sleep_timeout_ms = DEFAULT_SLEEP_TIMEOUT_MS;
  g_state.wake_cooldown_ms = DEFAULT_WAKE_COOLDOWN_MS;
  g_state.imu_wake_enabled = bb_imu_is_ready();
  g_state.message_wake_enabled = 1;

  /* 创建定时器（100ms 周期） */
  g_state.timeout_timer = xTimerCreate("sleep_timer", pdMS_TO_TICKS(100),
                                       pdTRUE, NULL, timeout_timer_cb);
  if (!g_state.timeout_timer) {
    ESP_LOGE(TAG, "Failed to create timeout timer");
    g_state.initialized = 0;
    return ESP_ERR_NO_MEM;
  }

  xTimerStart(g_state.timeout_timer, portMAX_DELAY);

  /* 注册 IMU 回调 */
  if (g_state.imu_wake_enabled) {
    bb_imu_on_sample(imu_sample_cb, NULL);
  }

  ESP_LOGI(TAG, "Sleep manager initialized (imu_wake=%d, message_wake=%d)",
           g_state.imu_wake_enabled, g_state.message_wake_enabled);

  return ESP_OK;
}

esp_err_t bb_sleep_manager_deinit(void) {
  if (!g_state.initialized) {
    return ESP_OK;
  }

  if (g_state.timeout_timer) {
    xTimerDelete(g_state.timeout_timer, portMAX_DELAY);
    g_state.timeout_timer = NULL;
  }

  bb_imu_on_sample_cancel();

  g_state.initialized = 0;
  ESP_LOGI(TAG, "Sleep manager deinitialized");
  return ESP_OK;
}

int bb_sleep_manager_is_ready(void) {
  return g_state.initialized;
}

bb_sleep_state_t bb_sleep_manager_get_state(void) {
  return g_state.state;
}

int bb_sleep_manager_is_sleeping(void) {
  return g_state.state == BB_SLEEP_STATE_SLEEPING;
}

uint32_t bb_sleep_manager_idle_time_ms(void) {
  uint32_t now = xTaskGetTickCount() * portTICK_PERIOD_MS;
  return now - g_state.last_activity_ms;
}

esp_err_t bb_sleep_manager_on_user_active(void) {
  if (!g_state.enabled) {
    return ESP_OK;
  }

  g_state.last_activity_ms = xTaskGetTickCount() * portTICK_PERIOD_MS;

  if (g_state.state != BB_SLEEP_STATE_ACTIVE) {
    transition_to_state(BB_SLEEP_STATE_ACTIVE);
    if (g_state.imu_wake_enabled) {
      bb_imu_disable_low_power();
    }
  }

  return ESP_OK;
}

esp_err_t bb_sleep_manager_on_message_arrived(void) {
  if (!g_state.enabled || !g_state.message_wake_enabled) {
    return ESP_OK;
  }

  if (g_state.state == BB_SLEEP_STATE_SLEEPING) {
    transition_to_state(BB_SLEEP_STATE_WAKING);
  }

  return ESP_OK;
}

esp_err_t bb_sleep_manager_manual_wake(void) {
  bb_sleep_manager_on_user_active();
  return ESP_OK;
}

esp_err_t bb_sleep_manager_manual_sleep(void) {
  if (!g_state.enabled) {
    return ESP_OK;
  }
  transition_to_state(BB_SLEEP_STATE_SLEEPING);
  return ESP_OK;
}

esp_err_t bb_sleep_manager_set_dimming_timeout_ms(uint32_t ms) {
  g_state.dimming_timeout_ms = ms;
  return ESP_OK;
}

esp_err_t bb_sleep_manager_set_sleep_timeout_ms(uint32_t ms) {
  g_state.sleep_timeout_ms = ms;
  return ESP_OK;
}

esp_err_t bb_sleep_manager_set_wake_cooldown_ms(uint32_t ms) {
  g_state.wake_cooldown_ms = ms;
  return ESP_OK;
}

esp_err_t bb_sleep_manager_set_imu_wake_enabled(int enable) {
  if (!bb_imu_is_ready() && enable) {
    return ESP_ERR_INVALID_STATE;
  }
  g_state.imu_wake_enabled = enable;
  return ESP_OK;
}

int bb_sleep_manager_is_imu_wake_enabled(void) {
  return g_state.imu_wake_enabled;
}

esp_err_t bb_sleep_manager_set_message_wake_enabled(int enable) {
  g_state.message_wake_enabled = enable;
  return ESP_OK;
}

esp_err_t bb_sleep_manager_set_enabled(int enable) {
  g_state.enabled = enable;
  if (enable) {
    bb_sleep_manager_on_user_active();
  }
  return ESP_OK;
}

int bb_sleep_manager_is_enabled(void) {
  return g_state.enabled;
}

esp_err_t bb_sleep_manager_dump_status(void) {
  ESP_LOGI(TAG, "=== Sleep Manager Status ===");
  ESP_LOGI(TAG, "State: %d (0=ACTIVE, 1=DIMMING, 2=SLEEPING, 3=WAKING)", g_state.state);
  ESP_LOGI(TAG, "Idle time: %ld ms", bb_sleep_manager_idle_time_ms());
  ESP_LOGI(TAG, "Brightness: level=%d raw=0x%02x",
           bb_display_get_brightness_level(), bb_display_get_brightness_raw());
  ESP_LOGI(TAG, "Enabled: %d, IMU wake: %d, Message wake: %d",
           g_state.enabled, g_state.imu_wake_enabled, g_state.message_wake_enabled);
  ESP_LOGI(TAG, "Timeouts: dimming=%ld ms, sleep=%ld ms, wake_cooldown=%ld ms",
           g_state.dimming_timeout_ms, g_state.sleep_timeout_ms, g_state.wake_cooldown_ms);
  return ESP_OK;
}
