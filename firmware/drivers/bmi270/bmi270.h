/**
 * Bosch BMI270 accelerometer driver (bbclaw, accel-only).
 *
 * 自成命名空间 —— 故意 NOT bb_imu_*：qmi8658.c 无条件编译且定义了全部 bb_imu_* 符号，
 * 再实现一份会重复符号链接错误。本驱动只读加速度（单位 mg），供体感导航手势用。
 *
 * 复用 bb_audio 已建的 port0 I2C 总线（M5StickS3：ES8311 0x18 / BMI270 0x68 /
 * M5PM1 0x6E 同 SDA47/SCL48），所以 bb_bmi270_init() 必须在 bb_audio_init() 之后调。
 */
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"

#ifdef __cplusplus
extern "C" {
#endif

/* 探 chip_id(0x24)→ soft reset → 灌 8192B config blob → 校验 INTERNAL_STATUS →
 * 开 accel（±2g / 100Hz / perf）。失败返回非 ESP_OK（手势模块据此降级为无体感）。 */
esp_err_t bb_bmi270_init(void);

/* 读加速度，单位毫g（milli-g，静止时朝上的轴 ~±1000）。未 init 返回 ESP_ERR_INVALID_STATE。 */
esp_err_t bb_bmi270_read_accel_mg(float *x_mg, float *y_mg, float *z_mg);

bool bb_bmi270_is_ready(void);

/* 8192 字节初始化配置 blob（drivers/bmi270/bmi270_config.c，Bosch BSD-3-Clause vendored）。 */
extern const uint8_t bmi270_config_file[];

#ifdef __cplusplus
}
#endif
