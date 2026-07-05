/**
 * 电源管理和低功耗集成层
 *
 * 协调初始化顺序：IMU → 亮度控制 → 息屏管理
 * 集成用户交互事件（PTT、触摸、消息）到息屏管理
 */

#include "bb_power_mgmt.h"
#include "bb_imu.h"
#include "bb_display_control.h"
#include "bb_sleep_manager.h"
#include "bb_board_config.h"

#include <esp_log.h>

static const char* TAG = "power_mgmt";

/**
 * 初始化电源管理子系统（IMU + 亮度控制 + 息屏管理）
 *
 * 初始化顺序严格：
 * 1. bb_display_brightness_init() — 显示亮度（底层）
 * 2. bb_imu_init() — IMU 采样任务
 * 3. bb_sleep_manager_init() — 状态机（依赖 1、2）
 *
 * @return ESP_OK 成功；ESP_ERR_NOT_SUPPORTED 硬件不支持
 */
esp_err_t bb_power_mgmt_init(void) {
  ESP_LOGI(TAG, "Initializing power management subsystem");

  /* 步骤 1: 初始化屏幕亮度控制 */
  esp_err_t ret = bb_display_brightness_init();
  if (ret != ESP_OK) {
    if (ret == ESP_ERR_NOT_SUPPORTED) {
      ESP_LOGW(TAG, "Display brightness control not supported on this platform");
    } else {
      ESP_LOGE(TAG, "Failed to initialize display brightness: %s", esp_err_to_name(ret));
      return ret;
    }
  }

  /* 步骤 2: 初始化 IMU */
  ret = bb_imu_init();
  if (ret != ESP_OK) {
    if (ret == ESP_ERR_NOT_SUPPORTED) {
      ESP_LOGW(TAG, "IMU not available, wake-on-motion disabled");
    } else {
      ESP_LOGE(TAG, "Failed to initialize IMU: %s", esp_err_to_name(ret));
      /* 不中断流程，继续初始化息屏管理（仅禁用 IMU 唤醒） */
    }
  }

  /* 步骤 3: 初始化息屏管理器 */
  if (bb_display_brightness_is_available()) {
    ret = bb_sleep_manager_init();
    if (ret != ESP_OK) {
      if (ret == ESP_ERR_NOT_SUPPORTED) {
        ESP_LOGW(TAG, "Sleep manager not available on this platform");
      } else {
        ESP_LOGE(TAG, "Failed to initialize sleep manager: %s", esp_err_to_name(ret));
      }
    } else {
      ESP_LOGI(TAG, "Sleep manager initialized successfully");
    }
  } else {
    ESP_LOGW(TAG, "Display brightness control unavailable, sleep manager disabled");
  }

  return ESP_OK;
}

/**
 * 清理电源管理资源（用于关机或系统重启）
 */
esp_err_t bb_power_mgmt_deinit(void) {
  ESP_LOGI(TAG, "Deinitializing power management");

  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_deinit();
  }

  if (bb_imu_is_ready()) {
    bb_imu_deinit();
  }

  if (bb_display_brightness_is_available()) {
    bb_display_brightness_deinit();
  }

  return ESP_OK;
}

/**
 * 通知用户正在交互（PTT、触摸等）。
 * 重置空闲计时器，唤醒屏幕（如睡眠中）。
 */
void bb_power_mgmt_on_user_activity(void) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_on_user_active();
  }
}

/**
 * 通知有消息到达（网络通知、聊天消息等）。
 * 如果屏幕睡眠，则唤醒到 WAKING 状态。
 */
void bb_power_mgmt_on_message_arrived(void) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_on_message_arrived();
  }
}

/**
 * 获取当前休眠状态（用于决策是否需要低功耗处理）。
 *
 * @return true 屏幕已息屏，false 屏幕亮着
 */
int bb_power_mgmt_is_sleeping(void) {
  if (bb_sleep_manager_is_ready()) {
    return bb_sleep_manager_is_sleeping();
  }
  return 0;
}

/**
 * 获取当前空闲时长（毫秒）。
 *
 * @return 毫秒数，0 表示刚刚有交互
 */
uint32_t bb_power_mgmt_idle_time_ms(void) {
  if (bb_sleep_manager_is_ready()) {
    return bb_sleep_manager_idle_time_ms();
  }
  return 0;
}

/**
 * 手动唤醒（用于 API 触发或外部唤醒源）。
 */
void bb_power_mgmt_manual_wake(void) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_manual_wake();
  }
}

/**
 * 手动睡眠（用于强制关屏）。
 */
void bb_power_mgmt_manual_sleep(void) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_manual_sleep();
  }
}

/**
 * 启用或禁用整个电源管理功能（用于调试或特定场景）。
 */
void bb_power_mgmt_set_enabled(int enable) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_set_enabled(enable);
  }
}

/**
 * 打印电源管理状态诊断信息。
 */
void bb_power_mgmt_dump_status(void) {
  ESP_LOGI(TAG, "=== Power Management Status ===");

  if (bb_display_brightness_is_available()) {
    ESP_LOGI(TAG, "Display: level=%d, raw=0x%02x, fading=%d",
             bb_display_get_brightness_level(),
             bb_display_get_brightness_raw(),
             bb_display_is_fading());
  } else {
    ESP_LOGI(TAG, "Display: not available");
  }

  if (bb_imu_is_ready()) {
    ESP_LOGI(TAG, "IMU: chip_id=0x%02x, rate=%dHz",
             bb_imu_get_chip_id(),
             bb_imu_get_sample_rate());
  } else {
    ESP_LOGI(TAG, "IMU: not available");
  }

  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_dump_status();
  } else {
    ESP_LOGI(TAG, "Sleep manager: not available");
  }
}
