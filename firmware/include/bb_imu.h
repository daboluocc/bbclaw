/**
 * IMU（惯性测量单元）通用接口 — 加速度计 + 陀螺仪
 *
 * 设计目标：跨芯片、跨板可插拔。具体实现（QMI8658、ICM42670、BMI270 等）
 * 无需改动此头文件，仅在 board_config.h 中启用对应芯片驱动即可。
 */
#pragma once

#include <stdint.h>
#include <esp_err.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ── IMU 数据结构 ── */

typedef struct {
  float x, y, z;  /* 单位：mg (1g ≈ 9.81 m/s²) */
} bb_imu_accel_t;

typedef struct {
  float x, y, z;  /* 单位：°/s (degree per second) */
} bb_imu_gyro_t;

typedef struct {
  uint64_t timestamp_us;       /* 采样时间戳 (微秒) */
  bb_imu_accel_t accel;        /* 加速度 */
  bb_imu_gyro_t gyro;          /* 角速度 */
} bb_imu_sample_t;

/* ── 回调函数类型（用于事件驱动） ── */

/**
 * IMU 新数据回调。当有新样本可读时调用（采样率驱动或中断驱动）。
 *
 * @param sample 最新样本
 * @param arg 用户自定义参数
 */
typedef void (*bb_imu_on_sample_cb_t)(const bb_imu_sample_t* sample, void* arg);

/**
 * IMU 事件回调。检测到特定事件时调用（如剧烈加速、倾转等）。
 *
 * @param event_id 事件 ID（由实现定义，如 WAKE_MOTION）
 * @param data 事件相关数据指针
 * @param arg 用户自定义参数
 */
typedef void (*bb_imu_on_event_cb_t)(uint32_t event_id, const void* data, void* arg);

/* ── IMU 事件 ID 定义（通用，不同芯片可扩展） ── */
#define BB_IMU_EVENT_MOTION        0x0001  /* 检测到运动（加速度突变） */
#define BB_IMU_EVENT_TILT          0x0002  /* 检测到倾转（角度变化） */
#define BB_IMU_EVENT_FREE_FALL     0x0003  /* 自由落体 */
#define BB_IMU_EVENT_SHAKE         0x0004  /* 摇晃（重复加速度） */
#define BB_IMU_EVENT_TAP           0x0005  /* 敲击 */

/* ── IMU 生命周期接口 ── */

/**
 * 初始化 IMU 硬件。
 *
 * @return ESP_OK 成功，否则返回错误码
 */
esp_err_t bb_imu_init(void);

/**
 * 反初始化 IMU，释放资源。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_deinit(void);

/**
 * 检查 IMU 是否已初始化。
 *
 * @return true 已初始化，false 未初始化或初始化失败
 */
int bb_imu_is_ready(void);

/* ── 数据读取接口 ── */

/**
 * 读取最新的 IMU 样本。
 *
 * @param out 输出样本结构
 * @return ESP_OK 成功，ESP_ERR_TIMEOUT 无新数据（非阻塞读取）
 */
esp_err_t bb_imu_read_sample(bb_imu_sample_t* out);

/**
 * 注册数据回调。当有新样本可用时触发，适合流式数据处理。
 *
 * @param cb 回调函数指针
 * @param arg 用户参数，原样传递给回调
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_on_sample(bb_imu_on_sample_cb_t cb, void* arg);

/**
 * 取消数据回调。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_on_sample_cancel(void);

/* ── 事件检测接口 ── */

/**
 * 注册事件回调。当检测到特定事件时触发（如剧烈运动）。
 *
 * @param event_id 要监听的事件 ID（如 BB_IMU_EVENT_MOTION）
 * @param cb 回调函数指针
 * @param arg 用户参数
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_on_event(uint32_t event_id, bb_imu_on_event_cb_t cb, void* arg);

/**
 * 取消事件回调。
 *
 * @param event_id 要取消的事件 ID
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_on_event_cancel(uint32_t event_id);

/* ── 配置接口 ── */

/**
 * 设置采样率（Hz）。
 *
 * @param hz 频率，如 100 表示 100Hz（10ms 采样周期）
 * @return ESP_OK 成功，ESP_ERR_INVALID_ARG 不支持的频率
 */
esp_err_t bb_imu_set_sample_rate(uint16_t hz);

/**
 * 获取当前采样率。
 *
 * @return 当前频率（Hz），0 表示未初始化
 */
uint16_t bb_imu_get_sample_rate(void);

/**
 * 设置加速度计量程（范围）。
 *
 * @param g 重力加速度倍数，如 8 表示 ±8g
 * @return ESP_OK 成功，ESP_ERR_INVALID_ARG 不支持的范围
 */
esp_err_t bb_imu_set_accel_range(uint8_t g);

/**
 * 获取当前加速度计量程。
 *
 * @return 量程（g），0 表示未初始化
 */
uint8_t bb_imu_get_accel_range(void);

/**
 * 设置陀螺仪量程。
 *
 * @param dps 度每秒，如 250 表示 ±250°/s
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_set_gyro_range(uint16_t dps);

/**
 * 获取当前陀螺仪量程。
 *
 * @return 量程（°/s），0 表示未初始化
 */
uint16_t bb_imu_get_gyro_range(void);

/**
 * 启用低功耗模式（降采样率、断电传感器等）。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_enable_low_power(void);

/**
 * 禁用低功耗模式，恢复正常采样。
 *
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_disable_low_power(void);

/* ── 诊断接口 ── */

/**
 * 获取 IMU 芯片 ID（用于验证硬件）。
 *
 * @return 芯片 ID，0 表示未初始化或不支持
 */
uint32_t bb_imu_get_chip_id(void);

/**
 * 获取原始数据（用于调试）。输出当前加速度和角速度的原始整数值。
 *
 * @param accel_raw 输出加速度原始值指针（3 个 int16_t）
 * @param gyro_raw 输出角速度原始值指针（3 个 int16_t）
 * @return ESP_OK 成功
 */
esp_err_t bb_imu_get_raw(int16_t* accel_raw, int16_t* gyro_raw);

#ifdef __cplusplus
}
#endif

