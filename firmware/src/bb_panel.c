/**
 * bb_panel.c — LCD panel init: SPI, i80, or QSPI (SH8601/CO5300 AMOLED),
 * selected by board config.
 */
#include "bb_panel.h"
#include "bb_config.h"

#include <esp_check.h>
#include <esp_log.h>
#include <esp_lcd_panel_ops.h>
#include <esp_lcd_panel_vendor.h>
#include <driver/gpio.h>

#if BBCLAW_DISPLAY_BUS_SPI || BBCLAW_DISPLAY_BUS_QSPI
#include <driver/spi_master.h>
#include <esp_lcd_panel_io.h>
#endif

#if BBCLAW_DISPLAY_BUS_I80
#include <esp_lcd_panel_io.h>
#endif

#if BBCLAW_DISPLAY_BUS_QSPI
#include "esp_lcd_sh8601.h"
#endif

static const char *TAG = "bb_panel";

/* ── SPI bus ── */
#if BBCLAW_DISPLAY_BUS_SPI
static esp_err_t init_spi(esp_lcd_panel_io_handle_t *io, esp_lcd_panel_handle_t *panel) {
    const spi_host_device_t host = (spi_host_device_t)BBCLAW_ST7789_HOST;
    spi_bus_config_t buscfg = {
        .sclk_io_num = BBCLAW_ST7789_SCLK_GPIO,
        .mosi_io_num = BBCLAW_ST7789_MOSI_GPIO,
        .miso_io_num = -1,
        .quadwp_io_num = -1,
        .quadhd_io_num = -1,
        .max_transfer_sz = BBCLAW_ST7789_WIDTH * 64 * (int)sizeof(uint16_t),
    };
    esp_err_t err = spi_bus_initialize(host, &buscfg, SPI_DMA_CH_AUTO);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) return err;

    esp_lcd_panel_io_spi_config_t io_config = {
        .dc_gpio_num = BBCLAW_ST7789_DC_GPIO,
        .cs_gpio_num = BBCLAW_ST7789_CS_GPIO,
        .pclk_hz = BBCLAW_ST7789_PCLK_HZ,
        .spi_mode = 0,
        .trans_queue_depth = 10,
        .lcd_cmd_bits = 8,
        .lcd_param_bits = 8,
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)host, &io_config, io),
                        TAG, "spi panel io");

    esp_lcd_panel_dev_config_t panel_config = {
        .reset_gpio_num = BBCLAW_ST7789_RST_GPIO,
#if BBCLAW_ST7789_RGB_ORDER_BGR
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_BGR,
#else
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_RGB,
#endif
        .bits_per_pixel = 16,
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_st7789(*io, &panel_config, panel), TAG, "spi st7789");
    return ESP_OK;
}
#endif /* BBCLAW_DISPLAY_BUS_SPI */

/* ── i80 parallel bus ── */
#if BBCLAW_DISPLAY_BUS_I80
static esp_err_t init_i80(esp_lcd_panel_io_handle_t *io, esp_lcd_panel_handle_t *panel) {
    /* Pull RD high (active-low read strobe, we only write) */
#if defined(BBCLAW_I80_RD_GPIO) && BBCLAW_I80_RD_GPIO >= 0
    gpio_config_t rd_cfg = {
        .pin_bit_mask = 1ULL << BBCLAW_I80_RD_GPIO,
        .mode = GPIO_MODE_OUTPUT,
        .pull_up_en = GPIO_PULLUP_ENABLE,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    gpio_config(&rd_cfg);
    gpio_set_level(BBCLAW_I80_RD_GPIO, 1);
#endif

    esp_lcd_i80_bus_handle_t i80_bus = NULL;
    esp_lcd_i80_bus_config_t bus_config = {
        .dc_gpio_num = BBCLAW_I80_DC_GPIO,
        .wr_gpio_num = BBCLAW_I80_WR_GPIO,
        .clk_src = LCD_CLK_SRC_DEFAULT,
        .data_gpio_nums = {
            BBCLAW_I80_D0_GPIO, BBCLAW_I80_D1_GPIO,
            BBCLAW_I80_D2_GPIO, BBCLAW_I80_D3_GPIO,
            BBCLAW_I80_D4_GPIO, BBCLAW_I80_D5_GPIO,
            BBCLAW_I80_D6_GPIO, BBCLAW_I80_D7_GPIO,
        },
        .bus_width = 8,
        .max_transfer_bytes = BBCLAW_ST7789_WIDTH * BBCLAW_ST7789_HEIGHT * (int)sizeof(uint16_t),
        .psram_trans_align = 64,
        .sram_trans_align = 4,
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_i80_bus(&bus_config, &i80_bus), TAG, "i80 bus");

    esp_lcd_panel_io_i80_config_t io_config = {
        .cs_gpio_num = BBCLAW_I80_CS_GPIO,
        .pclk_hz = BBCLAW_I80_PCLK_HZ,
        .trans_queue_depth = 10,
        .lcd_cmd_bits = 8,
        .lcd_param_bits = 8,
        .dc_levels = {
            .dc_idle_level = 0,
            .dc_cmd_level = 0,
            .dc_dummy_level = 0,
            .dc_data_level = 1,
        },
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_i80(i80_bus, &io_config, io), TAG, "i80 panel io");

    esp_lcd_panel_dev_config_t panel_config = {
        .reset_gpio_num = BBCLAW_ST7789_RST_GPIO,
#if BBCLAW_ST7789_RGB_ORDER_BGR
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_BGR,
#else
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_RGB,
#endif
        .bits_per_pixel = 16,
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_st7789(*io, &panel_config, panel), TAG, "i80 st7789");
    return ESP_OK;
}
#endif /* BBCLAW_DISPLAY_BUS_I80 */

/* ── QSPI bus: SH8601-compatible AMOLED (CO5300) ── */
#if BBCLAW_DISPLAY_BUS_QSPI
/* CO5300 init sequence（Waveshare BSP 原样）：sleep out → TE on → 显示窗口
 * (列 0x16–0x1AF / 行 0x00–0x1F5) → display on → 亮度 0x51 拉满 0xFF。
 * AMOLED 无背光 GPIO，运行期调亮度就是再发 0x51。 */
static const sh8601_lcd_init_cmd_t s_co5300_init_cmds[] = {
    {0x11, (uint8_t[]){0x00}, 0, 120},
    {0xC4, (uint8_t[]){0x80}, 1, 0},
    {0x44, (uint8_t[]){0x01, 0xD1}, 2, 0},
    {0x35, (uint8_t[]){0x00}, 1, 0},
    {0x53, (uint8_t[]){0x20}, 1, 10},
    {0x63, (uint8_t[]){0xFF}, 1, 10},
    {0x51, (uint8_t[]){0x00}, 1, 10},
    {0x2A, (uint8_t[]){0x00, 0x16, 0x01, 0xAF}, 4, 0},
    {0x2B, (uint8_t[]){0x00, 0x00, 0x01, 0xF5}, 4, 0},
    {0x29, (uint8_t[]){0x00}, 0, 10},
    {0x51, (uint8_t[]){0xFF}, 1, 0},
};

static esp_err_t init_qspi(esp_lcd_panel_io_handle_t *io, esp_lcd_panel_handle_t *panel) {
    const spi_host_device_t host = (spi_host_device_t)BBCLAW_QSPI_HOST;
    const spi_bus_config_t buscfg = SH8601_PANEL_BUS_QSPI_CONFIG(
        BBCLAW_QSPI_SCK_GPIO,
        BBCLAW_QSPI_D0_GPIO, BBCLAW_QSPI_D1_GPIO,
        BBCLAW_QSPI_D2_GPIO, BBCLAW_QSPI_D3_GPIO,
        BBCLAW_ST7789_WIDTH * BBCLAW_ST7789_HEIGHT * (int)sizeof(uint16_t));
    esp_err_t err = spi_bus_initialize(host, &buscfg, SPI_DMA_CH_AUTO);
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) return err;

    esp_lcd_panel_io_spi_config_t io_config =
        SH8601_PANEL_IO_QSPI_CONFIG(BBCLAW_QSPI_CS_GPIO, NULL, NULL);
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)host, &io_config, io),
                        TAG, "qspi panel io");

    sh8601_vendor_config_t vendor_config = {
        .init_cmds = s_co5300_init_cmds,
        .init_cmds_size = sizeof(s_co5300_init_cmds) / sizeof(s_co5300_init_cmds[0]),
        .flags = {
            .use_qspi_interface = 1,
        },
    };
    esp_lcd_panel_dev_config_t panel_config = {
        .reset_gpio_num = BBCLAW_QSPI_RST_GPIO,
#if BBCLAW_ST7789_RGB_ORDER_BGR
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_BGR,
#else
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_RGB,
#endif
        .bits_per_pixel = 16,
        .vendor_config = &vendor_config,
    };
    ESP_RETURN_ON_ERROR(esp_lcd_new_panel_sh8601(*io, &panel_config, panel), TAG, "qspi sh8601");
    return ESP_OK;
}
#endif /* BBCLAW_DISPLAY_BUS_QSPI */

/* ── Public ── */

esp_err_t bb_panel_init(esp_lcd_panel_io_handle_t *panel_io,
                        esp_lcd_panel_handle_t *panel) {
    if (panel_io == NULL || panel == NULL) return ESP_ERR_INVALID_ARG;

#if BBCLAW_DISPLAY_BUS_I80
    ESP_RETURN_ON_ERROR(init_i80(panel_io, panel), TAG, "i80 init");
#elif BBCLAW_DISPLAY_BUS_QSPI
    ESP_RETURN_ON_ERROR(init_qspi(panel_io, panel), TAG, "qspi init");
#elif BBCLAW_DISPLAY_BUS_SPI
    ESP_RETURN_ON_ERROR(init_spi(panel_io, panel), TAG, "spi init");
#else
#error "No display bus selected (BBCLAW_DISPLAY_BUS_SPI / BBCLAW_DISPLAY_BUS_I80 / BBCLAW_DISPLAY_BUS_QSPI)"
#endif

    ESP_RETURN_ON_ERROR(esp_lcd_panel_reset(*panel), TAG, "reset");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_init(*panel), TAG, "init");
#if BBCLAW_DISPLAY_BUS_QSPI
    /* SH8601/CO5300 不支持 swap_xy（无 MADCTL MV），mirror 也可能不支持——
     * 本板两者都配 0，NOT_SUPPORTED 视为可接受。 */
    {
        esp_err_t e = esp_lcd_panel_swap_xy(*panel, BBCLAW_ST7789_SWAP_XY);
        if (e != ESP_OK && e != ESP_ERR_NOT_SUPPORTED) return e;
        e = esp_lcd_panel_mirror(*panel, BBCLAW_ST7789_MIRROR_X, BBCLAW_ST7789_MIRROR_Y);
        if (e != ESP_OK && e != ESP_ERR_NOT_SUPPORTED) return e;
    }
#else
    ESP_RETURN_ON_ERROR(esp_lcd_panel_swap_xy(*panel, BBCLAW_ST7789_SWAP_XY), TAG, "swap_xy");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_mirror(*panel, BBCLAW_ST7789_MIRROR_X, BBCLAW_ST7789_MIRROR_Y), TAG, "mirror");
#endif
    ESP_RETURN_ON_ERROR(esp_lcd_panel_set_gap(*panel, BBCLAW_ST7789_X_GAP, BBCLAW_ST7789_Y_GAP), TAG, "gap");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_invert_color(*panel, BBCLAW_ST7789_INVERT_COLOR), TAG, "invert");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(*panel, true), TAG, "on");

    ESP_LOGI(TAG, "panel ready bus=%s wh=%dx%d swap_xy=%d mirror=(%d,%d) gap=(%d,%d) invert=%d",
             BBCLAW_DISPLAY_BUS_I80 ? "i80" : (BBCLAW_DISPLAY_BUS_QSPI ? "qspi" : "spi"),
             BBCLAW_ST7789_WIDTH, BBCLAW_ST7789_HEIGHT,
             BBCLAW_ST7789_SWAP_XY, BBCLAW_ST7789_MIRROR_X, BBCLAW_ST7789_MIRROR_Y,
             BBCLAW_ST7789_X_GAP, BBCLAW_ST7789_Y_GAP, BBCLAW_ST7789_INVERT_COLOR);
    return ESP_OK;
}
