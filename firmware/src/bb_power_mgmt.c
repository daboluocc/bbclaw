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
#include "bb_pm.h"
#include "bb_led.h"
#include "bb_config.h"

#include <esp_log.h>
#include <nvs.h>

static const char* TAG = "power_mgmt";

/* ── 息屏时间预设(用户可在设置页切换,存 NVS)──────────────────────────
 * {标签, 息屏秒数};0=永不息屏(disable 状态机)。变暗时间 = 息屏时间的 2/3。 */
#define SLEEP_NVS_NS   "bbpwr"
#define SLEEP_NVS_KEY  "sleep_preset"
#define SLEEP_PRESET_DEFAULT 3 /* 3 min,与历史默认一致 */
typedef struct {
  const char* label;
  int sleep_s;
} sleep_preset_t;
static const sleep_preset_t s_presets[] = {
    {"Never", 0}, {"30s", 30}, {"1 min", 60}, {"3 min", 180}, {"5 min", 300},
};
#define SLEEP_PRESET_COUNT ((int)(sizeof(s_presets) / sizeof(s_presets[0])))
static int s_sleep_preset = SLEEP_PRESET_DEFAULT;

/* 应用预设:只设息屏管理器字段(安全,任意上下文),不碰 NVS。 */
static void sleep_preset_apply(int idx) {
  if (idx < 0 || idx >= SLEEP_PRESET_COUNT) idx = SLEEP_PRESET_DEFAULT;
  s_sleep_preset = idx;
  if (!bb_sleep_manager_is_ready()) return;
  int ss = s_presets[idx].sleep_s;
  if (ss <= 0) {
    bb_sleep_manager_set_enabled(0); /* 永不息屏:禁状态机,屏常亮 */
  } else {
    bb_sleep_manager_set_enabled(1);
    bb_sleep_manager_set_sleep_timeout_ms((uint32_t)ss * 1000U);
    bb_sleep_manager_set_dimming_timeout_ms((uint32_t)ss * 1000U * 2U / 3U);
  }
  ESP_LOGI(TAG, "sleep preset -> %s (%ds)", s_presets[idx].label, ss);
}

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

  /* 从 NVS 读用户设定的息屏时间预设并应用(启动单线程期,NVS 读安全) */
  bb_power_mgmt_load_sleep_preset();

  /* 步骤 4: CPU/系统级低功耗(ADR-047,自动 light sleep + DFS)。门控化,未启用的板
   * 为 no-op。开机默认持交互锁(全响应),息屏转 SLEEPING 时由 tick 释放允许深睡。 */
  bb_pm_init();

  return ESP_OK;
}

int bb_power_mgmt_sleep_preset_count(void) { return SLEEP_PRESET_COUNT; }
int bb_power_mgmt_get_sleep_preset(void) { return s_sleep_preset; }
const char* bb_power_mgmt_sleep_preset_label(int idx) {
  if (idx < 0 || idx >= SLEEP_PRESET_COUNT) return "?";
  return s_presets[idx].label;
}

/* 循环到下一个预设并立即应用(只设字段,安全)。返回新 idx;NVS 落盘由调用方
 * 用 off-stream_task 的持久化任务做(NVS 写冻结 flash cache)。 */
int bb_power_mgmt_cycle_sleep_preset(void) {
  sleep_preset_apply((s_sleep_preset + 1) % SLEEP_PRESET_COUNT);
  return s_sleep_preset;
}

/* NVS 落盘。⚠️ 必须在内部栈任务上调用(NVS 写会冻结 flash cache,PSRAM 栈会崩)。 */
void bb_power_mgmt_save_sleep_preset(int idx) {
  nvs_handle_t h;
  if (nvs_open(SLEEP_NVS_NS, NVS_READWRITE, &h) != ESP_OK) return;
  (void)nvs_set_i32(h, SLEEP_NVS_KEY, idx);
  (void)nvs_commit(h);
  nvs_close(h);
}

void bb_power_mgmt_load_sleep_preset(void) {
  nvs_handle_t h;
  int32_t v = SLEEP_PRESET_DEFAULT;
  if (nvs_open(SLEEP_NVS_NS, NVS_READONLY, &h) == ESP_OK) {
    int32_t got = 0;
    if (nvs_get_i32(h, SLEEP_NVS_KEY, &got) == ESP_OK) v = got;
    nvs_close(h);
  }
  sleep_preset_apply((int)v);
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
void bb_power_mgmt_tick(void) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_tick();
    /* 息屏状态 → 低功耗联动(幂等,未启用板为 no-op):
     *  ① SLEEPING 释放交互锁允许 SoC light-sleep,其余态持锁保 UI/音频跟手;
     *  ② SLEEPING 灭状态灯——屏都黑了灯还亮着既费电又突兀(用户反馈),唤醒恢复。 */
    const int sleeping = bb_sleep_manager_is_sleeping();
    bb_pm_set_sleeping(sleeping);
    bb_led_set_suspended(sleeping);
  }
}

void bb_power_mgmt_debug_motion(void) {
  if (bb_sleep_manager_is_ready()) {
    bb_sleep_manager_debug_motion();
  }
}

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
