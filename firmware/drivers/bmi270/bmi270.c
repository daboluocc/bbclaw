/**
 * Bosch BMI270 accel-only driver — see bmi270.h. 寄存器时序移植自 Bosch
 * BMI270-Sensor-API（bmi2_soft_reset → write_config_file → 校验 INTERNAL_STATUS）。
 */
#include "bmi270.h"

#include <string.h>

#include "driver/i2c_master.h"
#include "esp_check.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "bb_audio.h"  /* bb_audio_shared_i2c_bus() */
#include "bb_config.h"

static const char *TAG = "bb_bmi270";

#ifndef BBCLAW_IMU_BMI270_I2C_ADDR
#define BBCLAW_IMU_BMI270_I2C_ADDR 0x68
#endif

/* ── BMI270 registers ── */
#define BMI2_CHIP_ID_ADDR    0x00
#define BMI2_CHIP_ID         0x24
#define BMI2_ACC_DATA_ADDR   0x0C /* 0x0C..0x11 = ACC_X/Y/Z LSB+MSB */
#define BMI2_INTERNAL_STATUS 0x21 /* &0x0F: 0x01=init ok */
#define BMI2_ACC_CONF_ADDR   0x40
#define BMI2_ACC_RANGE_ADDR  0x41
#define BMI2_INIT_CTRL_ADDR  0x59
#define BMI2_INIT_ADDR_0     0x5B
#define BMI2_INIT_ADDR_1     0x5C
#define BMI2_INIT_DATA_ADDR  0x5E
#define BMI2_PWR_CONF_ADDR   0x7C
#define BMI2_PWR_CTRL_ADDR   0x7D
#define BMI2_CMD_REG_ADDR    0x7E
#define BMI2_SOFT_RESET_CMD  0xB6

#define BMI2_CONFIG_SIZE 8192

static i2c_master_dev_handle_t s_dev;
static bool s_ready;

static esp_err_t wr8(uint8_t reg, uint8_t val) {
  uint8_t b[2] = {reg, val};
  return i2c_master_transmit(s_dev, b, sizeof(b), 1000);
}
static esp_err_t rd(uint8_t reg, uint8_t *buf, size_t len) {
  return i2c_master_transmit_receive(s_dev, &reg, 1, buf, len, 1000);
}

/* 灌 8192B config blob：INIT_DATA(0x5E) 是 address-trapped，按 Bosch 做法分块——
 * 每块先写 INIT_ADDR(0x5B/0x5C = 字偏移)，再 burst 到 0x5E。256B/块共 32 块。 */
static esp_err_t upload_config(void) {
  const size_t CHUNK = 256; /* even */
  static uint8_t txbuf[1 + 256];
  for (size_t off = 0; off < BMI2_CONFIG_SIZE; off += CHUNK) {
    size_t word = off / 2;
    ESP_RETURN_ON_ERROR(wr8(BMI2_INIT_ADDR_0, (uint8_t)(word & 0x0F)), TAG, "init_addr0");
    ESP_RETURN_ON_ERROR(wr8(BMI2_INIT_ADDR_1, (uint8_t)((word >> 4) & 0xFF)), TAG, "init_addr1");
    txbuf[0] = BMI2_INIT_DATA_ADDR;
    memcpy(&txbuf[1], &bmi270_config_file[off], CHUNK);
    ESP_RETURN_ON_ERROR(i2c_master_transmit(s_dev, txbuf, 1 + CHUNK, 1000), TAG, "init_data");
  }
  return ESP_OK;
}

esp_err_t bb_bmi270_init(void) {
  if (s_ready) return ESP_OK; /* 幂等：wake/nav 模块可能都调 */
  s_ready = false;
  i2c_master_bus_handle_t bus = bb_audio_shared_i2c_bus();
  if (!bus) {
    ESP_LOGE(TAG, "shared i2c bus not ready (must init after bb_audio_init)");
    return ESP_ERR_INVALID_STATE;
  }
  if (!s_dev) {
    i2c_device_config_t cfg = {
        .dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = BBCLAW_IMU_BMI270_I2C_ADDR,
        .scl_speed_hz = 400000,
    };
    ESP_RETURN_ON_ERROR(i2c_master_bus_add_device(bus, &cfg, &s_dev), TAG, "add dev");
  }

  uint8_t id = 0;
  if (rd(BMI2_CHIP_ID_ADDR, &id, 1) != ESP_OK || id != BMI2_CHIP_ID) {
    ESP_LOGE(TAG, "chip_id=0x%02X (expect 0x24) — BMI270 not found", id);
    return ESP_ERR_NOT_FOUND;
  }

  (void)wr8(BMI2_CMD_REG_ADDR, BMI2_SOFT_RESET_CMD); /* soft reset */
  vTaskDelay(pdMS_TO_TICKS(5));
  (void)rd(BMI2_CHIP_ID_ADDR, &id, 1); /* dummy read: keep I2C selected after reset */

  ESP_RETURN_ON_ERROR(wr8(BMI2_PWR_CONF_ADDR, 0x00), TAG, "pwr_conf aps off"); /* MANDATORY before load */
  vTaskDelay(pdMS_TO_TICKS(1));
  ESP_RETURN_ON_ERROR(wr8(BMI2_INIT_CTRL_ADDR, 0x00), TAG, "init_ctrl prep");
  ESP_RETURN_ON_ERROR(upload_config(), TAG, "config upload");
  ESP_RETURN_ON_ERROR(wr8(BMI2_INIT_CTRL_ADDR, 0x01), TAG, "init_ctrl commit");
  vTaskDelay(pdMS_TO_TICKS(25));

  uint8_t st = 0;
  ESP_RETURN_ON_ERROR(rd(BMI2_INTERNAL_STATUS, &st, 1), TAG, "internal_status");
  if ((st & 0x0F) != 0x01) {
    ESP_LOGE(TAG, "config init failed, INTERNAL_STATUS=0x%02X (0x00=not_init 0x02=err)", st);
    return ESP_FAIL;
  }

  ESP_RETURN_ON_ERROR(wr8(BMI2_PWR_CTRL_ADDR, 0x04), TAG, "pwr_ctrl acc_en"); /* accel only */
  ESP_RETURN_ON_ERROR(wr8(BMI2_ACC_CONF_ADDR, 0xA8), TAG, "acc_conf");        /* perf|avg4|100Hz */
  ESP_RETURN_ON_ERROR(wr8(BMI2_ACC_RANGE_ADDR, 0x00), TAG, "acc_range");      /* ±2g → 16384 LSB/g */
  ESP_RETURN_ON_ERROR(wr8(BMI2_PWR_CONF_ADDR, 0x02), TAG, "pwr_conf run");    /* APS=0(轮询必须) fifo_self_wakeup=1 */
  vTaskDelay(pdMS_TO_TICKS(20));

  s_ready = true;
  ESP_LOGI(TAG, "BMI270 init ok (accel ±2g/100Hz)");
  return ESP_OK;
}

esp_err_t bb_bmi270_read_accel_mg(float *x_mg, float *y_mg, float *z_mg) {
  if (!s_ready) return ESP_ERR_INVALID_STATE;
  uint8_t b[6];
  ESP_RETURN_ON_ERROR(rd(BMI2_ACC_DATA_ADDR, b, sizeof(b)), TAG, "read accel");
  int16_t rx = (int16_t)((uint16_t)b[1] << 8 | b[0]);
  int16_t ry = (int16_t)((uint16_t)b[3] << 8 | b[2]);
  int16_t rz = (int16_t)((uint16_t)b[5] << 8 | b[4]);
  const float k = 1000.0f / 16384.0f; /* ±2g range */
  if (x_mg) *x_mg = rx * k;
  if (y_mg) *y_mg = ry * k;
  if (z_mg) *z_mg = rz * k;
  return ESP_OK;
}

bool bb_bmi270_is_ready(void) { return s_ready; }
