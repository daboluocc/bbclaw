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

/** 周期 tick(stream_task 输入循环调用)——见 bb_sleep_manager_tick。 */
void bb_power_mgmt_tick(void);

/** devmon 测试注入:模拟 IMU 运动唤醒。 */
void bb_power_mgmt_debug_motion(void);

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
 * 屏幕是否处于「未完全点亮」的休眠/暗屏态（DIMMING/SLEEPING/WAKING）。
 * 比 is_sleeping 更广，供「暗屏时按 OK 只唤醒、不触发动作」判据。
 *
 * @return 1 变暗/息屏/唤醒中，0 完全点亮(ACTIVE)或无息屏管理
 */
int bb_power_mgmt_is_resting(void);

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

/* ── 息屏时间预设(设置页可调,存 NVS)── */

/** 预设总数。 */
int bb_power_mgmt_sleep_preset_count(void);
/** 当前预设 idx。 */
int bb_power_mgmt_get_sleep_preset(void);
/** 预设 idx 的显示标签(Never/30s/1 min/3 min/5 min)。 */
const char* bb_power_mgmt_sleep_preset_label(int idx);
/** 切到下一个预设并立即应用;返回新 idx。NVS 落盘请另用 save(内部栈)。 */
int bb_power_mgmt_cycle_sleep_preset(void);
/** 把预设 idx 写入 NVS。⚠️ 必须在内部栈任务上调用(NVS 写冻结 flash cache)。 */
void bb_power_mgmt_save_sleep_preset(int idx);
/** 开机从 NVS 读预设并应用(单线程期调用)。 */
void bb_power_mgmt_load_sleep_preset(void);

/* ── 诊断 ── */

/**
 * 打印所有电源管理子系统的状态信息到日志。
 */
void bb_power_mgmt_dump_status(void);

#ifdef __cplusplus
}
#endif
