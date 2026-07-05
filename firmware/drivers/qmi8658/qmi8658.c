/**
 * QMI8658 六轴 IMU 驱动实现
 *
 * 提供 bb_imu.h 接口的具体实现。支持 I2C 通信，采样率 8-512Hz。
 */

#include "qmi8658.h"
#include "bb_imu.h"
#include "bb_board_config.h"

#include <string.h>
#include <esp_log.h>
#include <driver/i2c_master.h>
#include <freertos/FreeRTOS.h>
#include <freertos/queue.h>
#include <freertos/task.h>

static const char* TAG = "qmi8658";

/* ── 全局状态 ── */
static qmi8658_state_t g_state = {0};
static i2c_master_dev_handle_t g_i2c_dev_handle = NULL;
static QueueHandle_t g_sample_queue = NULL;
static TaskHandle_t g_sample_task_handle = NULL;
static bb_imu_on_sample_cb_t g_sample_cb = NULL;
static void* g_sample_cb_arg = NULL;

/* ── 寄存器 I/O ── */

esp_err_t qmi8658_read_reg(uint8_t addr, uint8_t* out) {
  if (!g_i2c_dev_handle || !out) {
    return ESP_ERR_INVALID_ARG;
  }
  return i2c_master_transmit_receive(g_i2c_dev_handle, &addr, 1, out, 1, -1);
}

esp_err_t qmi8658_read_regs(uint8_t addr, uint8_t* buf, uint8_t len) {
  if (!g_i2c_dev_handle || !buf || len == 0) {
    return ESP_ERR_INVALID_ARG;
  }
  return i2c_master_transmit_receive(g_i2c_dev_handle, &addr, 1, buf, len, -1);
}

esp_err_t qmi8658_write_reg(uint8_t addr, uint8_t val) {
  if (!g_i2c_dev_handle) {
    return ESP_ERR_INVALID_ARG;
  }
  uint8_t buf[2] = {addr, val};
  return i2c_master_transmit(g_i2c_dev_handle, buf, 2, -1);
}

esp_err_t qmi8658_read_accel_raw(int16_t* x, int16_t* y, int16_t* z) {
  uint8_t buf[6];
  esp_err_t ret = qmi8658_read_regs(QMI8658_REG_ACCEL_X_L, buf, 6);
  if (ret == ESP_OK) {
    *x = (int16_t)((buf[1] << 8) | buf[0]);
    *y = (int16_t)((buf[3] << 8) | buf[2]);
    *z = (int16_t)((buf[5] << 8) | buf[4]);
  }
  return ret;
}

esp_err_t qmi8658_read_gyro_raw(int16_t* x, int16_t* y, int16_t* z) {
  uint8_t buf[6];
  esp_err_t ret = qmi8658_read_regs(QMI8658_REG_GYRO_X_L, buf, 6);
  if (ret == ESP_OK) {
    *x = (int16_t)((buf[1] << 8) | buf[0]);
    *y = (int16_t)((buf[3] << 8) | buf[2]);
    *z = (int16_t)((buf[5] << 8) | buf[4]);
  }
  return ret;
}

/* ── 数据采样任务 ── */

static void qmi8658_sample_task(void* arg) {
  bb_imu_sample_t sample;
  TickType_t delay = pdMS_TO_TICKS(1000 / g_state.sample_rate_hz);

  ESP_LOGI(TAG, "Sample task started (rate=%dHz, delay=%d ticks)",
           g_state.sample_rate_hz, delay);

  while (g_state.initialized) {
    vTaskDelay(delay);

    int16_t accel_raw[3], gyro_raw[3];
    if (qmi8658_read_accel_raw(&accel_raw[0], &accel_raw[1], &accel_raw[2]) != ESP_OK ||
        qmi8658_read_gyro_raw(&gyro_raw[0], &gyro_raw[1], &gyro_raw[2]) != ESP_OK) {
      ESP_LOGW(TAG, "Failed to read IMU data");
      continue;
    }

    /* 转换原始数据为物理单位 */
    sample.timestamp_us = esp_timer_get_time();
    sample.accel.x = accel_raw[0] * g_state.accel_scale;
    sample.accel.y = accel_raw[1] * g_state.accel_scale;
    sample.accel.z = accel_raw[2] * g_state.accel_scale;
    sample.gyro.x = gyro_raw[0] * g_state.gyro_scale;
    sample.gyro.y = gyro_raw[1] * g_state.gyro_scale;
    sample.gyro.z = gyro_raw[2] * g_state.gyro_scale;

    /* 发送到队列供消费者读取 */
    if (g_sample_queue) {
      xQueueOverwrite(g_sample_queue, &sample);
    }

    /* 触发注册的回调 */
    if (g_sample_cb) {
      g_sample_cb(&sample, g_sample_cb_arg);
    }
  }

  vTaskDelete(NULL);
}

/* ── 寄存器初始化 ── */

static esp_err_t qmi8658_init_registers(void) {
  /* 软复位 */
  esp_err_t ret = qmi8658_write_reg(0x60, 0xB0);  /* CTRL9 = soft reset */
  if (ret != ESP_OK) return ret;
  vTaskDelay(pdMS_TO_TICKS(10));

  /* 配置采样率和量程 */
  /* CTRL1: ODR (bits 6-4) */
  uint8_t ctrl1 = QMI8658_ODR_128HZ;  /* 128Hz 采样率 */
  ret = qmi8658_write_reg(QMI8658_REG_CTRL1, ctrl1);
  if (ret != ESP_OK) return ret;

  /* CTRL2: 加速度计范围和启用 */
  uint8_t ctrl2 = (QMI8658_ACCEL_RANGE_8G << 5) | 0x01;  /* ±8g, enable */
  ret = qmi8658_write_reg(QMI8658_REG_CTRL2, ctrl2);
  if (ret != ESP_OK) return ret;

  /* CTRL3: 陀螺仪范围和启用 */
  uint8_t ctrl3 = (QMI8658_GYRO_RANGE_256DPS << 5) | 0x01;  /* ±256°/s, enable */
  ret = qmi8658_write_reg(QMI8658_REG_CTRL3, ctrl3);
  if (ret != ESP_OK) return ret;

  /* CTRL7: 启用加速度计和陀螺仪数据就绪中断 */
  uint8_t ctrl7 = 0x00;  /* INT1 = data ready */
  ret = qmi8658_write_reg(QMI8658_REG_CTRL7, ctrl7);
  if (ret != ESP_OK) return ret;

  ESP_LOGI(TAG, "QMI8658 registers configured: ODR=128Hz, Accel=±8g, Gyro=±256°/s");
  return ESP_OK;
}

/* ── 公共接口实现 ── */

esp_err_t bb_imu_init(void) {
#ifndef BBCLAW_IMU_ENABLE
  ESP_LOGI(TAG, "IMU disabled by board config");
  return ESP_ERR_NOT_SUPPORTED;
#endif

  if (g_state.initialized) {
    return ESP_OK;
  }

  /* 获取 I2C 总线句柄（由 bb_audio 初始化和导出） */
  extern i2c_master_bus_handle_t bb_audio_shared_i2c_bus(void);
  i2c_master_bus_handle_t bus_handle = bb_audio_shared_i2c_bus();
  if (!bus_handle) {
    ESP_LOGE(TAG, "I2C bus not initialized");
    return ESP_ERR_INVALID_STATE;
  }

  i2c_device_config_t dev_config = {
    .dev_addr_length = I2C_ADDR_BIT_7,
    .device_address = BBCLAW_IMU_QMI8658_I2C_ADDR,
    .scl_speed_hz = 400000,
  };

  esp_err_t ret = i2c_master_bus_add_device(bus_handle, &dev_config, &g_i2c_dev_handle);
  if (ret != ESP_OK) {
    ESP_LOGE(TAG, "Failed to add I2C device: %s", esp_err_to_name(ret));
    return ret;
  }

  /* 验证芯片 ID */
  uint8_t chip_id;
  ret = qmi8658_read_reg(QMI8658_WHOAMI, &chip_id);
  if (ret != ESP_OK) {
    ESP_LOGE(TAG, "Failed to read chip ID: %s", esp_err_to_name(ret));
    return ret;
  }

  if (chip_id != 0x05) {
    ESP_LOGE(TAG, "Unexpected chip ID: 0x%02x (expected 0x05)", chip_id);
    return ESP_ERR_NOT_FOUND;
  }

  ESP_LOGI(TAG, "QMI8658 chip detected (ID: 0x%02x)", chip_id);

  /* 初始化配置 */
  g_state.initialized = 1;
  g_state.i2c_addr = BBCLAW_IMU_QMI8658_I2C_ADDR;
  g_state.sample_rate_hz = BBCLAW_IMU_SAMPLE_RATE_HZ;
  g_state.accel_range_g = 8;    /* 默认 ±8g */
  g_state.gyro_range_dps = 256;  /* 默认 ±256°/s */
  g_state.accel_scale = QMI8658_ACCEL_CONV_8G;
  g_state.gyro_scale = QMI8658_GYRO_CONV_256DPS;

  /* 配置寄存器（采样率、量程等） */
  ret = qmi8658_init_registers();
  if (ret != ESP_OK) {
    ESP_LOGE(TAG, "Failed to initialize registers: %s", esp_err_to_name(ret));
    g_state.initialized = 0;
    return ret;
  }

  /* 创建采样队列和采样任务 */
  g_sample_queue = xQueueCreate(1, sizeof(bb_imu_sample_t));
  if (!g_sample_queue) {
    ESP_LOGE(TAG, "Failed to create sample queue");
    g_state.initialized = 0;
    return ESP_ERR_NO_MEM;
  }

  ret = xTaskCreate(qmi8658_sample_task, "qmi8658_sample", 4096, NULL, 5, &g_sample_task_handle);
  if (ret != pdPASS) {
    ESP_LOGE(TAG, "Failed to create sample task");
    vQueueDelete(g_sample_queue);
    g_sample_queue = NULL;
    g_state.initialized = 0;
    return ESP_ERR_NO_MEM;
  }

  ESP_LOGI(TAG, "IMU initialized: rate=%dHz, accel=±%dg, gyro=±%d°/s",
           g_state.sample_rate_hz, g_state.accel_range_g, g_state.gyro_range_dps);

  return ESP_OK;
}

esp_err_t bb_imu_deinit(void) {
  if (!g_state.initialized) {
    return ESP_OK;
  }

  g_state.initialized = 0;

  if (g_sample_task_handle) {
    vTaskDelete(g_sample_task_handle);
    g_sample_task_handle = NULL;
  }

  if (g_sample_queue) {
    vQueueDelete(g_sample_queue);
    g_sample_queue = NULL;
  }

  if (g_i2c_dev_handle) {
    i2c_master_bus_rm_device(g_i2c_dev_handle);
    g_i2c_dev_handle = NULL;
  }

  g_sample_cb = NULL;
  g_sample_cb_arg = NULL;

  ESP_LOGI(TAG, "IMU deinitialized");
  return ESP_OK;
}

int bb_imu_is_ready(void) {
  return g_state.initialized;
}

esp_err_t bb_imu_read_sample(bb_imu_sample_t* out) {
  if (!out || !g_sample_queue) {
    return ESP_ERR_INVALID_ARG;
  }
  if (xQueuePeek(g_sample_queue, out, 0) == pdTRUE) {
    return ESP_OK;
  }
  return ESP_ERR_TIMEOUT;
}

esp_err_t bb_imu_on_sample(bb_imu_on_sample_cb_t cb, void* arg) {
  if (!cb) {
    return ESP_ERR_INVALID_ARG;
  }
  g_sample_cb = cb;
  g_sample_cb_arg = arg;
  return ESP_OK;
}

esp_err_t bb_imu_on_sample_cancel(void) {
  g_sample_cb = NULL;
  g_sample_cb_arg = NULL;
  return ESP_OK;
}

/* 其他接口的 stub 实现（后续补全） */

esp_err_t bb_imu_on_event(uint32_t event_id, bb_imu_on_event_cb_t cb, void* arg) {
  // TODO: 实现事件检测
  return ESP_ERR_NOT_SUPPORTED;
}

esp_err_t bb_imu_on_event_cancel(uint32_t event_id) {
  return ESP_OK;
}

esp_err_t bb_imu_set_sample_rate(uint16_t hz) {
  /* 将 Hz 映射到 CTRL1 ODR 字段 */
  uint8_t odr;
  if (hz <= 8) odr = QMI8658_ODR_8HZ;
  else if (hz <= 16) odr = QMI8658_ODR_16HZ;
  else if (hz <= 32) odr = QMI8658_ODR_32HZ;
  else if (hz <= 64) odr = QMI8658_ODR_64HZ;
  else if (hz <= 128) odr = QMI8658_ODR_128HZ;
  else if (hz <= 256) odr = QMI8658_ODR_256HZ;
  else if (hz <= 512) odr = QMI8658_ODR_512HZ;
  else return ESP_ERR_INVALID_ARG;

  uint8_t ctrl1 = odr;
  esp_err_t ret = qmi8658_write_reg(QMI8658_REG_CTRL1, ctrl1);
  if (ret == ESP_OK) {
    g_state.sample_rate_hz = hz;
  }
  return ret;
}

uint16_t bb_imu_get_sample_rate(void) {
  return g_state.sample_rate_hz;
}

esp_err_t bb_imu_set_accel_range(uint8_t g) {
  uint8_t range_code;
  if (g == 2) range_code = QMI8658_ACCEL_RANGE_2G;
  else if (g == 4) range_code = QMI8658_ACCEL_RANGE_4G;
  else if (g == 8) range_code = QMI8658_ACCEL_RANGE_8G;
  else if (g == 16) range_code = QMI8658_ACCEL_RANGE_16G;
  else return ESP_ERR_INVALID_ARG;

  uint8_t ctrl2 = (range_code << 5) | 0x01;
  esp_err_t ret = qmi8658_write_reg(QMI8658_REG_CTRL2, ctrl2);
  if (ret == ESP_OK) {
    g_state.accel_range_g = g;
    g_state.accel_scale = (g == 2) ? QMI8658_ACCEL_CONV_2G :
                          (g == 4) ? QMI8658_ACCEL_CONV_4G :
                          (g == 8) ? QMI8658_ACCEL_CONV_8G :
                          QMI8658_ACCEL_CONV_16G;
  }
  return ret;
}

uint8_t bb_imu_get_accel_range(void) {
  return g_state.accel_range_g;
}

esp_err_t bb_imu_set_gyro_range(uint16_t dps) {
  uint8_t range_code;
  if (dps == 64) range_code = QMI8658_GYRO_RANGE_64DPS;
  else if (dps == 128) range_code = QMI8658_GYRO_RANGE_128DPS;
  else if (dps == 256) range_code = QMI8658_GYRO_RANGE_256DPS;
  else if (dps == 512) range_code = QMI8658_GYRO_RANGE_512DPS;
  else return ESP_ERR_INVALID_ARG;

  uint8_t ctrl3 = (range_code << 5) | 0x01;
  esp_err_t ret = qmi8658_write_reg(QMI8658_REG_CTRL3, ctrl3);
  if (ret == ESP_OK) {
    g_state.gyro_range_dps = dps;
    g_state.gyro_scale = (dps == 64) ? QMI8658_GYRO_CONV_64DPS :
                         (dps == 128) ? QMI8658_GYRO_CONV_128DPS :
                         (dps == 256) ? QMI8658_GYRO_CONV_256DPS :
                         QMI8658_GYRO_CONV_512DPS;
  }
  return ret;
}

uint16_t bb_imu_get_gyro_range(void) {
  return g_state.gyro_range_dps;
}

esp_err_t bb_imu_enable_low_power(void) {
  /* 降采样率到 16Hz，减少功耗 */
  return bb_imu_set_sample_rate(16);
}

esp_err_t bb_imu_disable_low_power(void) {
  /* 恢复配置的采样率 */
  return bb_imu_set_sample_rate(BBCLAW_IMU_SAMPLE_RATE_HZ);
}

uint32_t bb_imu_get_chip_id(void) {
  return g_state.initialized ? 0x05 : 0;
}

esp_err_t bb_imu_get_raw(int16_t* accel_raw, int16_t* gyro_raw) {
  if (!accel_raw || !gyro_raw) {
    return ESP_ERR_INVALID_ARG;
  }
  return qmi8658_read_accel_raw(&accel_raw[0], &accel_raw[1], &accel_raw[2]) ||
         qmi8658_read_gyro_raw(&gyro_raw[0], &gyro_raw[1], &gyro_raw[2]);
}
