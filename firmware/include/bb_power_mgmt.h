/**
 * 电源管理和低功耗集成层接口
 *
 * 简化应用层集成，隐藏 IMU、亮度控制、息屏管理的复杂度。
 * 提供统一的初始化、事件通知、状态查询接口。
 */
#pragma once

#include <stdint.h>
#include <esp_err.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ── 生命周期管理 ── */

/**
 * 初始化电源管理子系统（IMU + 亮度控制 + 息屏管理）
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_power_mgmt_init(void);

/**
 * 清理电源管理资源
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_power_mgmt_deinit(void);

/* ── 事件通知 ── */

/**
 * 用户交互事件（PTT 按下、触摸屏幕、接收聊天消息等）。
 * 重置空闲计时器，立即唤醒屏幕到 ACTIVE 状态。
 */
void bb_power_mgmt_on_user_activity(void);

/**
 * 消息到达事件（网络推送通知、来电提醒等）。
 * 如果屏幕睡眠，唤醒到 WAKING 状态；如果已活跃，保持不变。
 */
void bb_power_mgmt_on_message_arrived(void);

/* ── 状态查询 ── */

/**
 * 检查屏幕是否已息屏。
 *
 * @return true 屏幕关闭，false 屏幕亮着
 */
int bb_power_mgmt_is_sleeping(void);

/**
 * 获取从上次用户交互至今的空闲时长（毫秒）。
 *
 * @return 毫秒数
 */
uint32_t bb_power_mgmt_idle_time_ms(void);

/* ── 手动控制 ── */

/**
 * 手动唤醒屏幕（绕过逻辑，立即转 ACTIVE）。
 * 用于 API 触发或特殊场景。
 */
void bb_power_mgmt_manual_wake(void);

/**
 * 手动关屏（立即转 SLEEPING）。
 */
void bb_power_mgmt_manual_sleep(void);

/**
 * 启用或禁用电源管理（用于调试或特定工作模式）。
 * 禁用时屏幕保持常亮，IMU 继续工作。
 */
void bb_power_mgmt_set_enabled(int enable);

/* ── 诊断 ── */

/**
 * 打印所有电源管理子系统的状态信息到日志。
 */
void bb_power_mgmt_dump_status(void);

#ifdef __cplusplus
}
#endif
