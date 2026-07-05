/**
 * 息屏管理器 — IMU + 屏幕亮度联动状态机
 *
 * 根据用户交互和 IMU 运动检测自动管理屏幕亮度。
 * 支持多种工作模式，可跨板配置。
 */
#pragma once

#include <stdint.h>
#include <esp_err.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ── 睡眠管理状态 ── */

typedef enum {
  BB_SLEEP_STATE_ACTIVE = 0,    /* 用户交互中，屏幕全亮 */
  BB_SLEEP_STATE_DIMMING,       /* 空闲过渡，屏幕变暗 */
  BB_SLEEP_STATE_SLEEPING,      /* 息屏，低功耗待机 */
  BB_SLEEP_STATE_WAKING,        /* IMU 检测到唤醒信号，屏幕渐亮 */
} bb_sleep_state_t;

/* ── 事件类型 ── */

typedef enum {
  BB_SLEEP_EVENT_USER_ACTIVE,   /* 用户交互（按键/触摸/网络消息） */
  BB_SLEEP_EVENT_IMU_MOTION,    /* IMU 检测到运动 */
  BB_SLEEP_EVENT_TIMEOUT,       /* 空闲超时 */
  BB_SLEEP_EVENT_MANUAL_WAKE,   /* 手动唤醒（API 调用） */
} bb_sleep_event_t;

/* ── 生命周期接口 ── */

/**
 * 初始化息屏管理器。必须在 bb_display_brightness_init() 和 bb_imu_init() 之后调用。
 *
 * @return ESP_OK 成功；ESP_ERR_INVALID_STATE 依赖组件未初始化
 */
esp_err_t bb_sleep_manager_init(void);

/**
 * 反初始化息屏管理器，停止所有定时器和回调。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_deinit(void);

/**
 * 检查息屏管理器是否已初始化。
 *
 * @return true 已初始化，false 未初始化
 */
int bb_sleep_manager_is_ready(void);

/* ── 状态查询 ── */

/**
 * 获取当前睡眠状态。
 *
 * @return 当前状态枚举值
 */
bb_sleep_state_t bb_sleep_manager_get_state(void);

/**
 * 检查屏幕是否已息屏。
 *
 * @return true 屏幕关闭，false 屏幕亮着
 */
int bb_sleep_manager_is_sleeping(void);

/**
 * 获取从上一次用户交互至今经过的时间（毫秒）。
 *
 * @return 毫秒数；0 表示刚刚有交互
 */
uint32_t bb_sleep_manager_idle_time_ms(void);

/* ── 事件驱动 ── */

/**
 * 通知息屏管理器有用户交互事件。会重置空闲计时器并唤醒屏幕（如需）。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_on_user_active(void);

/**
 * 通知有网络消息到达。根据配置可能触发屏幕唤醒。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_on_message_arrived(void);

/**
 * 手动唤醒屏幕（绕过所有逻辑，立即转 ACTIVE）。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_manual_wake(void);

/**
 * 手动进入睡眠（立即关闭屏幕）。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_manual_sleep(void);

/* ── 配置接口 ── */

/**
 * 设置 ACTIVE → DIMMING 的超时时间。
 *
 * @param ms 毫秒数，如 120000 表示 2 分钟
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_set_dimming_timeout_ms(uint32_t ms);

/**
 * 设置 DIMMING → SLEEPING 的超时时间（总共从 ACTIVE 开始的时长）。
 *
 * @param ms 毫秒数，如 180000 表示 3 分钟
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_set_sleep_timeout_ms(uint32_t ms);

/**
 * 设置 IMU 唤醒的去抖延迟（从 SLEEPING 进入 WAKING 后的稳定时间）。
 *
 * @param ms 毫秒数，如 2000 表示 2 秒
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_set_wake_cooldown_ms(uint32_t ms);

/**
 * 启用或禁用 IMU 唤醒功能。
 *
 * @param enable 1 启用，0 禁用
 * @return ESP_OK 成功；ESP_ERR_INVALID_STATE IMU 未初始化
 */
esp_err_t bb_sleep_manager_set_imu_wake_enabled(int enable);

/**
 * 检查 IMU 唤醒功能是否已启用。
 *
 * @return 1 已启用，0 已禁用
 */
int bb_sleep_manager_is_imu_wake_enabled(void);

/**
 * 启用或禁用网络消息唤醒。
 *
 * @param enable 1 启用，0 禁用
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_set_message_wake_enabled(int enable);

/**
 * 启用或禁用整个息屏管理功能（便于调试/禁用）。
 *
 * @param enable 1 启用，0 禁用
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_set_enabled(int enable);

/**
 * 检查息屏管理功能是否已启用。
 *
 * @return 1 已启用，0 已禁用
 */
int bb_sleep_manager_is_enabled(void);

/* ── 诊断接口 ── */

/**
 * 打印当前状态到日志（调试用）。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_sleep_manager_dump_status(void);

#ifdef __cplusplus
}
#endif

#endif  /* BB_SLEEP_MANAGER_H */
