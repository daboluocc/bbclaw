/**
 * bb_pca9557.c — Minimal PCA9557 I2C IO expander driver（仿 bb_xl9555.c）.
 *
 * 实战派板：bit0 = LCD_CS（低=选通，唯一 SPI 设备故常选通）、bit1 = PA_EN。
 * 显示 init 早于音频 init，故本驱动 get-or-create 共享 I2C 总线；bb_audio
 * 的 es8311 路径同样 get-or-create，两边顺序无关。
 */
#include "bb_pca9557.h"
#include "bb_config.h"

#if BBCLAW_PCA9557_ENABLE

#include <esp_check.h>
#include <esp_log.h>
#include <driver/i2c_master.h>

static const char *TAG = "bb_pca9557";

static i2c_master_dev_handle_t s_dev;
static i2c_master_bus_handle_t s_bus;
static int s_ready;

/* PCA9557 registers */
#define REG_INPUT    0x00
#define REG_OUTPUT   0x01
#define REG_POLARITY 0x02
#define REG_CONFIG   0x03 /* 1 = input, 0 = output */

static esp_err_t write_reg(uint8_t reg, uint8_t val) {
    uint8_t buf[2] = {reg, val};
    return i2c_master_transmit(s_dev, buf, sizeof(buf), 100);
}

static esp_err_t read_reg(uint8_t reg, uint8_t *val) {
    ESP_RETURN_ON_ERROR(i2c_master_transmit(s_dev, &reg, 1, 100), TAG, "tx");
    ESP_RETURN_ON_ERROR(i2c_master_receive(s_dev, val, 1, 100), TAG, "rx");
    return ESP_OK;
}

esp_err_t bb_pca9557_init(void) {
    if (s_ready) return ESP_OK;

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
        .device_address = BBCLAW_PCA9557_I2C_ADDR,
        .scl_speed_hz = 100000,
    };
    ESP_RETURN_ON_ERROR(i2c_master_bus_add_device(s_bus, &dev_cfg, &s_dev), TAG, "add pca9557");

    /* 先写输出值再开输出方向，避免方向切换瞬间的电平毛刺：
       LCD_CS=0（选通）、PA_EN=0（关功放，音频起来后再开） */
    ESP_RETURN_ON_ERROR(write_reg(REG_OUTPUT, 0x00), TAG, "output init");
    const uint8_t cfg = (uint8_t)~((1U << BBCLAW_PCA9557_LCD_CS_BIT) | (1U << BBCLAW_PCA9557_PA_EN_BIT));
    ESP_RETURN_ON_ERROR(write_reg(REG_CONFIG, cfg), TAG, "cfg dir");

    s_ready = 1;
    /* 回读自证：确认方向/输出位真的落进了芯片（bring-up 排障关键证据）。
     * 注意此日志在 CDC tee 启动前发出，通常只在 UART0 可见。 */
    uint8_t out_rb = 0xEE, cfg_rb = 0xEE;
    (void)read_reg(REG_OUTPUT, &out_rb);
    (void)read_reg(REG_CONFIG, &cfg_rb);
    ESP_LOGI(TAG, "pca9557 ready addr=0x%02x (lcd_cs=bit%d pa_en=bit%d) readback out=0x%02x cfg=0x%02x",
             BBCLAW_PCA9557_I2C_ADDR, BBCLAW_PCA9557_LCD_CS_BIT, BBCLAW_PCA9557_PA_EN_BIT, out_rb, cfg_rb);
    return ESP_OK;
}

esp_err_t bb_pca9557_set_output(uint8_t bit, uint8_t level) {
    if (!s_ready) return ESP_ERR_INVALID_STATE;
    uint8_t val = 0;
    ESP_RETURN_ON_ERROR(read_reg(REG_OUTPUT, &val), TAG, "read output");
    val = (val & ~(1U << bit)) | ((level ? 1U : 0U) << bit);
    ESP_RETURN_ON_ERROR(write_reg(REG_OUTPUT, val), TAG, "write output");
    return ESP_OK;
}

#else /* !BBCLAW_PCA9557_ENABLE */

esp_err_t bb_pca9557_init(void) { return ESP_OK; }
esp_err_t bb_pca9557_set_output(uint8_t bit, uint8_t level) { (void)bit; (void)level; return ESP_OK; }

#endif
