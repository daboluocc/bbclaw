/**
 * 息屏管理器实现 — 状态机 + IMU + 屏幕亮度联动
 *
 * 根据用户交互和 IMU 运动自动管理屏幕亮度，降低功耗。
 */

#include "bb_sleep_manager.h"
#include "bb_imu.h"
#include "bb_display_control.h"
#include "bb_config.h"

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
  uint32_t wake_detected_ms;
  /* 跨任务事件旗标(IMU 任务/LVGL 触摸/WS 任务只置位,所有状态转换统一在
   * bb_sleep_manager_tick——stream_task 上执行。曾经的 100ms FreeRTOS 定时器
   * 在 Tmr Svc 小栈跑亮度渐变+任务击杀,uxListRemove StoreProhibited 随机崩
   * (2026-07-06 终案),该架构禁止回归)。 */
  volatile int motion_pending;
  volatile int message_pending;
  volatile int activity_pending;
  uint32_t last_tick_ms;
} sleep_manager_state_t;

static sleep_manager_state_t g_state = {0};

/* ── 默认配置值 ── */
#define DEFAULT_DIMMING_TIMEOUT_MS   (2 * 60 * 1000)   /* 2 min */
#define DEFAULT_SLEEP_TIMEOUT_MS     (3 * 60 * 1000)   /* 3 min */
#define DEFAULT_WAKE_COOLDOWN_MS     2000
/* QMI8658 样本单位是 m/s²(qmi8658.h 转换 *9.81/32768,原注释 mg 是错的),
 * 静止时 a_total≈重力 9.81。抬手唤醒 = 总加速度偏离重力基线超过阈值(抬腕瞬间
 * 叠加线加速度,幅值偏离 9.81)。原阈值 1500(当 mg 用)≈153g,±8g 传感器物理
 * 永不可达 → 抬手唤醒长期失效。改为偏离量判定,灵敏度真机可调。 */
#define GRAVITY_MS2                  9.81f
#define DEFAULT_IMU_MOTION_DELTA_MS2 2.5f

/* ── 状态转换 ── */

static void transition_to_state(bb_sleep_state_t new_state) {
  if (g_state.state == new_state) {
    return;
  }

  const bb_sleep_state_t old_state = g_state.state;
  ESP_LOGI(TAG, "State transition: %d → %d", g_state.state, new_state);

  /* 离开 SLEEPING:先 DISPON 把面板开回来,否则随后 fade 亮度也显示不出来。 */
  if (old_state == BB_SLEEP_STATE_SLEEPING && new_state != BB_SLEEP_STATE_SLEEPING) {
    bb_display_set_panel_on(1);
  }

  switch (new_state) {
    case BB_SLEEP_STATE_ACTIVE:
      bb_display_fade_brightness(BB_BRIGHTNESS_MAX, 200);
      break;

    case BB_SLEEP_STATE_DIMMING:
      bb_display_fade_brightness(BB_BRIGHTNESS_LOW, 500);
      break;

    case BB_SLEEP_STATE_SLEEPING:
      bb_display_fade_brightness(BB_BRIGHTNESS_OFF, 1000);
      /* 0x51 写 0 熄不灭 CO5300 AMOLED(仍扫描发光)——DISPOFF 才真正黑屏。
       * fade 已把亮度降到 0(唤醒时从黑淡入),DISPOFF 停止面板输出近零功耗。 */
      bb_display_set_panel_on(0);
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

/* ── 周期 tick(stream_task 输入循环调用,节流 100ms)── */

void bb_sleep_manager_tick(void) {
  if (!g_state.initialized || !g_state.enabled) {
    return;
  }
  uint32_t now = xTaskGetTickCount() * portTICK_PERIOD_MS;
  if (now - g_state.last_tick_ms < 100U) {
    return;
  }
  g_state.last_tick_ms = now;

  /* 消费跨任务事件旗标(转换只在本上下文做) */
  if (g_state.activity_pending) {
    g_state.activity_pending = 0;
    if (g_state.state != BB_SLEEP_STATE_ACTIVE) {
      transition_to_state(BB_SLEEP_STATE_ACTIVE);
      if (g_state.imu_wake_enabled) {
        bb_imu_disable_low_power();
      }
    }
  }
  if (g_state.motion_pending) {
    g_state.motion_pending = 0;
    if (g_state.state == BB_SLEEP_STATE_SLEEPING) {
      ESP_LOGI(TAG, "IMU motion wake");
      transition_to_state(BB_SLEEP_STATE_WAKING);
    }
  }
  if (g_state.message_pending) {
    g_state.message_pending = 0;
    if (g_state.message_wake_enabled && g_state.state == BB_SLEEP_STATE_SLEEPING) {
      transition_to_state(BB_SLEEP_STATE_WAKING);
    }
  }

  /* 带符号差:last_activity_ms/wake_detected_ms 可能被其他上下文(触摸/本 tick
   * 内的转换)更新得比本轮 now 更新,无符号减法下溢成 42 亿 → 秒睡/秒回睡
   * (真机实锤:motion 唤醒 1ms 后被打回 SLEEPING)。 */
  int32_t idle_time = (int32_t)(now - g_state.last_activity_ms);
  switch (g_state.state) {
    case BB_SLEEP_STATE_ACTIVE:
      if (idle_time >= (int32_t)g_state.dimming_timeout_ms) {
        transition_to_state(BB_SLEEP_STATE_DIMMING);
      }
      break;

    case BB_SLEEP_STATE_DIMMING:
      if (idle_time >= (int32_t)g_state.sleep_timeout_ms) {
        transition_to_state(BB_SLEEP_STATE_SLEEPING);
      }
      break;

    case BB_SLEEP_STATE_WAKING:
      /* 唤醒去抖窗口内无真实交互 → 回到睡眠(原实现拿 idle 时长与时间戳
       * 相加值比较,恒假,WAKING 永不回睡) */
      if ((int32_t)(now - g_state.wake_detected_ms) >= (int32_t)g_state.wake_cooldown_ms) {
        transition_to_state(BB_SLEEP_STATE_SLEEPING);
      }
      break;

    case BB_SLEEP_STATE_SLEEPING:
      break;
  }
}

/* devmon 测试注入:模拟一次 IMU 运动(与真实回调同一旗标通路) */
void bb_sleep_manager_debug_motion(void) {
  g_state.motion_pending = 1;
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

  /* 偏离重力基线检测——只置旗标,转换由 tick 在 stream_task 做(IMU 采样任务
   * 栈小,不许在这里碰亮度/QSPI)。用 |a_total - g| 而非 a_total>阈值:后者在
   * m/s² 量纲下静止就恒 9.81,取任何 >9.81 的绝对阈值都会被静止态的抖动误触发或
   * 完全不触发,偏离量才是「有没有在动」的正确度量。 */
  if (fabsf(a_total - GRAVITY_MS2) > DEFAULT_IMU_MOTION_DELTA_MS2) {
    g_state.motion_pending = 1;
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
  /* 可能从 LVGL(触摸)/stream_task 多上下文进来:只记时间戳+置旗标,
   * 转换由 tick 统一执行(最大延迟一个 tick 节流周期 100ms)。 */
  g_state.last_activity_ms = xTaskGetTickCount() * portTICK_PERIOD_MS;
  if (g_state.state != BB_SLEEP_STATE_ACTIVE) {
    g_state.activity_pending = 1;
  }
  return ESP_OK;
}

esp_err_t bb_sleep_manager_on_message_arrived(void) {
  if (!g_state.enabled || !g_state.message_wake_enabled) {
    return ESP_OK;
  }

  if (g_state.state == BB_SLEEP_STATE_SLEEPING) {
    g_state.message_pending = 1;
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
