/**
 * QMI8658 六轴 IMU 驱动（私有头文件）
 *
 * Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板集成的运动传感器
 * 地址: 0x6B (默认) / 0x6A (AD0 接地)
 */
#pragma once

#include <stdint.h>
#include <esp_err.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ── I2C 配置 ── */
#define QMI8658_I2C_ADDR_DEFAULT    0x6B
#define QMI8658_I2C_ADDR_ALT        0x6A

/* ── 芯片 ID 和寄存器地址 ── */
#define QMI8658_CHIP_ID             0x00
#define QMI8658_WHOAMI              0x00  /* 读取返回 0x05 = QMI8658A */

#define QMI8658_REG_CTRL1           0x02
#define QMI8658_REG_CTRL2           0x03
#define QMI8658_REG_CTRL3           0x04
#define QMI8658_REG_CTRL7           0x08
#define QMI8658_REG_CTRL8           0x09

#define QMI8658_REG_ACCEL_X_L       0x11
#define QMI8658_REG_ACCEL_X_H       0x12
#define QMI8658_REG_ACCEL_Y_L       0x13
#define QMI8658_REG_ACCEL_Y_H       0x14
#define QMI8658_REG_ACCEL_Z_L       0x15
#define QMI8658_REG_ACCEL_Z_H       0x16

#define QMI8658_REG_GYRO_X_L        0x17
#define QMI8658_REG_GYRO_X_H        0x18
#define QMI8658_REG_GYRO_Y_L        0x19
#define QMI8658_REG_GYRO_Y_H        0x1A
#define QMI8658_REG_GYRO_Z_L        0x1B
#define QMI8658_REG_GYRO_Z_H        0x1C

#define QMI8658_REG_STATUS          0x2D  /* INT 状态标志 */
#define QMI8658_REG_STATUS_INT_SRC  0x2E

/* ── 加速度计量程配置 (CTRL2) ── */
#define QMI8658_ACCEL_RANGE_2G      0x00
#define QMI8658_ACCEL_RANGE_4G      0x01
#define QMI8658_ACCEL_RANGE_8G      0x02
#define QMI8658_ACCEL_RANGE_16G     0x03

/* ── 陀螺仪量程配置 (CTRL3) ── */
#define QMI8658_GYRO_RANGE_64DPS    0x00
#define QMI8658_GYRO_RANGE_128DPS   0x01
#define QMI8658_GYRO_RANGE_256DPS   0x02
#define QMI8658_GYRO_RANGE_512DPS   0x03

/* ── 采样率配置 (CTRL1) ── */
#define QMI8658_ODR_8HZ             0x00
#define QMI8658_ODR_16HZ            0x01
#define QMI8658_ODR_32HZ            0x02
#define QMI8658_ODR_64HZ            0x03
#define QMI8658_ODR_128HZ           0x04
#define QMI8658_ODR_256HZ           0x05
#define QMI8658_ODR_512HZ           0x06

/* ── 转换系数 ── */
/* 加速度计：16 位有符号，量程转换 */
#define QMI8658_ACCEL_CONV_2G       (2.0f * 9.81f / 32768.0f)   /* mg */
#define QMI8658_ACCEL_CONV_4G       (4.0f * 9.81f / 32768.0f)
#define QMI8658_ACCEL_CONV_8G       (8.0f * 9.81f / 32768.0f)
#define QMI8658_ACCEL_CONV_16G      (16.0f * 9.81f / 32768.0f)

/* 陀螺仪：16 位有符号，单位 °/s */
#define QMI8658_GYRO_CONV_64DPS     (64.0f / 32768.0f)
#define QMI8658_GYRO_CONV_128DPS    (128.0f / 32768.0f)
#define QMI8658_GYRO_CONV_256DPS    (256.0f / 32768.0f)
#define QMI8658_GYRO_CONV_512DPS    (512.0f / 32768.0f)

/* ── 内部状态 ── */
typedef struct {
  int initialized;
  uint8_t i2c_addr;
  uint16_t sample_rate_hz;
  uint8_t accel_range_g;
  uint16_t gyro_range_dps;
  float accel_scale;
  float gyro_scale;
} qmi8658_state_t;

/* ── 内部函数 ── */

/**
 * 通过 I2C 读取单个寄存器。
 *
 * @param addr 寄存器地址
 * @param out 输出字节
 * @return ESP_OK 成功
 */
esp_err_t qmi8658_read_reg(uint8_t addr, uint8_t* out);

/**
 * 通过 I2C 读取多个寄存器。
 *
 * @param addr 起始寄存器地址
 * @param buf 输出缓冲区
 * @param len 读取字节数
 * @return ESP_OK 成功
 */
esp_err_t qmi8658_read_regs(uint8_t addr, uint8_t* buf, uint8_t len);

/**
 * 通过 I2C 写入单个寄存器。
 *
 * @param addr 寄存器地址
 * @param val 要写入的值
 * @return ESP_OK 成功
 */
esp_err_t qmi8658_write_reg(uint8_t addr, uint8_t val);

/**
 * 从 16 个寄存器读取加速度计原始数据（6 字节）。
 *
 * @param x 输出 X 轴原始值
 * @param y 输出 Y 轴原始值
 * @param z 输出 Z 轴原始值
 * @return ESP_OK 成功
 */
esp_err_t qmi8658_read_accel_raw(int16_t* x, int16_t* y, int16_t* z);

/**
 * 读取陀螺仪原始数据（6 字节）。
 *
 * @param x 输出 X 轴原始值
 * @param y 输出 Y 轴原始值
 * @param z 输出 Z 轴原始值
 * @return ESP_OK 成功
 */
esp_err_t qmi8658_read_gyro_raw(int16_t* x, int16_t* y, int16_t* z);

#ifdef __cplusplus
}
#endif

#endif  /* QMI8658_H */
