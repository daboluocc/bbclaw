/**
 * bb_xl9555.c — Minimal XL9555 I2C IO expander driver.
 *
 * Only implements output-set for speaker/amp enable bits.
 * Uses legacy i2c_master API matching the shared bus from bb_audio (ES8311 path).
 */
#include "bb_xl9555.h"
#include "bb_config.h"

#if BBCLAW_XL9555_ENABLE

#include <esp_check.h>
#include <esp_log.h>
#include <driver/i2c_master.h>

static const char *TAG = "bb_xl9555";

static i2c_master_dev_handle_t s_dev;
static i2c_master_bus_handle_t s_bus;

/* XL9555 registers */
#define REG_OUTPUT_PORT0 0x02
#define REG_OUTPUT_PORT1 0x03
#define REG_CONFIG_PORT0 0x06
#define REG_CONFIG_PORT1 0x07

static esp_err_t write_reg(uint8_t reg, uint8_t val) {
    uint8_t buf[2] = {reg, val};
    return i2c_master_transmit(s_dev, buf, sizeof(buf), 100);
}

static esp_err_t read_reg(uint8_t reg, uint8_t *val) {
    ESP_RETURN_ON_ERROR(i2c_master_transmit(s_dev, &reg, 1, 100), TAG, "tx");
    ESP_RETURN_ON_ERROR(i2c_master_receive(s_dev, val, 1, 100), TAG, "rx");
    return ESP_OK;
}

esp_err_t bb_xl9555_init(void) {
    /* Try to get existing bus (e.g. from ES8311 audio init), or create one */
    esp_err_t bus_err = i2c_master_get_bus_handle(BBCLAW_ES8311_I2C_PORT, &s_bus);
    if (bus_err != ESP_OK) {
        i2c_master_bus_config_t bus_cfg = {
            .i2c_port = BBCLAW_ES8311_I2C_PORT,
            .sda_io_num = BBCLAW_ES8311_I2C_SDA_GPIO,
            .scl_io_num = BBCLAW_ES8311_I2C_SCL_GPIO,
            .clk_source = I2C_CLK_SRC_DEFAULT,
            .glitch_ignore_cnt = 7,
            .flags.enable_internal_pullup = true,
        };
        ESP_RETURN_ON_ERROR(i2c_new_master_bus(&bus_cfg, &s_bus), TAG, "i2c bus create");
    }

    i2c_device_config_t dev_cfg = {
        .dev_addr_length = I2C_ADDR_BIT_LEN_7,
        .device_address = BBCLAW_XL9555_I2C_ADDR,
        .scl_speed_hz = 100000,
    };
    ESP_RETURN_ON_ERROR(i2c_master_bus_add_device(s_bus, &dev_cfg, &s_dev), TAG, "add xl9555");

    /* Configure port 0: bits 5,7 as output (0), rest input (1) → 0x1B
       Configure port 1: bit 0 as output (0), rest input (1) → 0xFE */
    ESP_RETURN_ON_ERROR(write_reg(REG_CONFIG_PORT0, 0x1B), TAG, "cfg port0");
    ESP_RETURN_ON_ERROR(write_reg(REG_CONFIG_PORT1, 0xFE), TAG, "cfg port1");

    ESP_LOGI(TAG, "xl9555 ready addr=0x%02x", BBCLAW_XL9555_I2C_ADDR);
    return ESP_OK;
}

esp_err_t bb_xl9555_set_output(uint8_t bit, uint8_t level) {
    uint8_t reg = (bit < 8) ? REG_OUTPUT_PORT0 : REG_OUTPUT_PORT1;
    uint8_t idx = (bit < 8) ? bit : (bit - 8);
    uint8_t val = 0;
    ESP_RETURN_ON_ERROR(read_reg(reg, &val), TAG, "read port");
    val = (val & ~(1U << idx)) | ((level ? 1U : 0U) << idx);
    ESP_RETURN_ON_ERROR(write_reg(reg, val), TAG, "write port");
    return ESP_OK;
}

#else /* !BBCLAW_XL9555_ENABLE */

esp_err_t bb_xl9555_init(void) { return ESP_OK; }
esp_err_t bb_xl9555_set_output(uint8_t bit, uint8_t level) { (void)bit; (void)level; return ESP_OK; }

#endif
