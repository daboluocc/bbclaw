/**
 * 体感导航（tilt gesture nav）—— 用 BMI270 加速度计把设备倾斜映射为 UP/DOWN/LEFT/RIGHT
 * 导航事件，喂进 bb_nav_input_inject()。配合侧键（OK/长按 BACK）= M5StickS3 的完整交互。
 *
 * 仅在 BBCLAW_IMU_BMI270_NAV=1 的板启用；bb_bmi270_init 失败则降级为无体感（不影响其它输入）。
 * 必须在 bb_audio_init()（建 I2C 总线）与 bb_nav_input_init() 之后调用。
 */
#pragma once

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

esp_err_t bb_nav_imu_init(void);

#ifdef __cplusplus
}
#endif
