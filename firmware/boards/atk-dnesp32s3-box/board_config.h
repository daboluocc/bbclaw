/**
 * BBClaw board config: ATK-DNESP32S3-BOX
 *
 * Alientek ESP32-S3 development box with:
 *   - ES8311 codec (or NS4168 fallback) via I2C + I2S duplex
 *   - XL9555 I2C IO expander (SPK enable, codec detect)
 *   - ST7789 320x240 8-bit i80 parallel interface
 *   - BOOT button on GPIO0
 *   - Single LED on GPIO4
 *
 * Reference: xiaozhi-esp32/main/boards/atk-dnesp32s3-box/
 */
#pragma once

/* ── Audio: I2S duplex (NS4168/INMP441-compatible, no codec register control) ── */
#define BBCLAW_AUDIO_INPUT_SOURCE "inmp441"
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

/* I2S pins (duplex: shared BCLK/WS, separate DIN/DOUT) */
#define BBCLAW_ES8311_I2S_MCK_GPIO -1
#define BBCLAW_ES8311_I2S_BCK_GPIO 21
#define BBCLAW_ES8311_I2S_WS_GPIO  13
#define BBCLAW_ES8311_I2S_DO_GPIO  14
#define BBCLAW_ES8311_I2S_DI_GPIO  47

/* I2C bus (shared with XL9555; ES8311 codec control not used in inmp441 mode) */
#define BBCLAW_ES8311_I2C_PORT     0
#define BBCLAW_ES8311_I2C_SDA_GPIO 48
#define BBCLAW_ES8311_I2C_SCL_GPIO 45
#define BBCLAW_ES8311_I2C_ADDR     0x18

/* ── XL9555 IO expander ── */
#define BBCLAW_XL9555_ENABLE       1
#define BBCLAW_XL9555_I2C_ADDR     0x20
/* SPK_CTRL on XL9555 port 0 bit 5; amp enable on port 0 bit 7 (via xl9555_cfg) */
#define BBCLAW_XL9555_SPK_EN_BIT   5
#define BBCLAW_XL9555_AMP_EN_BIT   7

/* ── PTT: BOOT button ── */
#define BBCLAW_PTT_GPIO         0
#define BBCLAW_PTT_ACTIVE_LEVEL 0
#define BBCLAW_PTT_PULL_UP      1

/* ── Motor: not available ── */
#define BBCLAW_MOTOR_ENABLE     0
#define BBCLAW_MOTOR_GPIO       -1

/* ── Status LED: single LED on GPIO4 ── */
#define BBCLAW_STATUS_LED_ENABLE 1
#define BBCLAW_STATUS_LED_KIND_RGB_MODULE 0
#define BBCLAW_STATUS_LED_R_GPIO 4
#define BBCLAW_STATUS_LED_Y_GPIO 4
#define BBCLAW_STATUS_LED_G_GPIO 4
#define BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE 0

/* ── Display: i80 parallel ST7789 320x240 ── */
#define BBCLAW_DISPLAY_BUS_SPI   0
#define BBCLAW_DISPLAY_BUS_I80   1

#define BBCLAW_ST7789_WIDTH    320
#define BBCLAW_ST7789_HEIGHT   240
#define BBCLAW_ST7789_RST_GPIO -1   /* NC */
#define BBCLAW_ST7789_BL_GPIO  -1   /* no backlight control pin */

/* i80 bus pins */
#define BBCLAW_I80_CS_GPIO      1
#define BBCLAW_I80_DC_GPIO      2
#define BBCLAW_I80_WR_GPIO     42
#define BBCLAW_I80_RD_GPIO     41
#define BBCLAW_I80_D0_GPIO     40
#define BBCLAW_I80_D1_GPIO     39
#define BBCLAW_I80_D2_GPIO     38
#define BBCLAW_I80_D3_GPIO     12
#define BBCLAW_I80_D4_GPIO     11
#define BBCLAW_I80_D5_GPIO     10
#define BBCLAW_I80_D6_GPIO      9
#define BBCLAW_I80_D7_GPIO     46
#define BBCLAW_I80_PCLK_HZ    (10 * 1000 * 1000)

/* Display orientation */
#define BBCLAW_ST7789_SWAP_XY       1
#define BBCLAW_ST7789_MIRROR_X      1
#define BBCLAW_ST7789_MIRROR_Y      0
#define BBCLAW_ST7789_INVERT_COLOR  1
#define BBCLAW_ST7789_RGB_ORDER_BGR 0
#define BBCLAW_ST7789_SWAP_BYTES    0
#define BBCLAW_ST7789_X_GAP         0
#define BBCLAW_ST7789_Y_GAP         0
#define BBCLAW_ST7789_PCLK_HZ      BBCLAW_I80_PCLK_HZ

/* ── PA enable: controlled via XL9555, not direct GPIO ── */
#define BBCLAW_PA_EN_GPIO      -1
