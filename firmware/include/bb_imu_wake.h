/**
 * BMI270 拿起/运动唤醒 —— 轮询加速度，检测到"在动"就注入 motion 到 bb_sleep_manager，
 * 睡眠中即唤醒亮屏（放下静止后由 sleep manager 的空闲超时自动息屏）。
 *
 * 仅 BBCLAW_IMU_BMI270_WAKE=1 的板启用；bb_bmi270_init 失败则降级（不影响其它功能）。
 * 必须在 bb_audio_init()（建 I2C 总线）之后调用。
 */
#pragma once

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

esp_err_t bb_imu_wake_init(void);

#ifdef __cplusplus
}
#endif
