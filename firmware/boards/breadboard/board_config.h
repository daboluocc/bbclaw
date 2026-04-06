/**
 * BBClaw board config: breadboard (INMP441 + MAX98357A + SPI ST7789 1.47")
 *
 * This is the default development configuration using discrete modules
 * wired on a breadboard with an ESP32-S3 devkit.
 */
#pragma once

/* ── Audio: INMP441 mic + MAX98357A amp, shared I2S bus ── */
#define BBCLAW_AUDIO_INPUT_SOURCE "inmp441"
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

#define BBCLAW_ES8311_I2S_BCK_GPIO 16
#define BBCLAW_ES8311_I2S_WS_GPIO  15
#define BBCLAW_ES8311_I2S_DO_GPIO  17
#define BBCLAW_ES8311_I2S_DI_GPIO  18
#define BBCLAW_ES8311_I2S_MCK_GPIO  2

/* ── PTT ── */
#define BBCLAW_PTT_GPIO        7
#define BBCLAW_PTT_ACTIVE_LEVEL 1
#define BBCLAW_PTT_PULL_UP     0

/* ── Motor ── */
#define BBCLAW_MOTOR_GPIO      21
#define BBCLAW_MOTOR_ENABLE    1

/* ── Status LED (discrete R/Y/G) ── */
#define BBCLAW_STATUS_LED_ENABLE 1
#define BBCLAW_STATUS_LED_KIND_RGB_MODULE 1
#define BBCLAW_STATUS_LED_R_GPIO 2
#define BBCLAW_STATUS_LED_Y_GPIO 4
#define BBCLAW_STATUS_LED_G_GPIO 5

/* ── Display: SPI ST7789 1.47" 172x320 ── */
#define BBCLAW_DISPLAY_BUS_SPI   1
#define BBCLAW_DISPLAY_BUS_I80   0

#define BBCLAW_ST7789_HOST       2
#define BBCLAW_ST7789_SCLK_GPIO 12
#define BBCLAW_ST7789_MOSI_GPIO 11
#define BBCLAW_ST7789_CS_GPIO   10
#define BBCLAW_ST7789_DC_GPIO    9
#define BBCLAW_ST7789_RST_GPIO  14
#define BBCLAW_ST7789_BL_GPIO   13
#define BBCLAW_ST7789_WIDTH    320
#define BBCLAW_ST7789_HEIGHT   172

/*
 * Panel variant: 1 = V1 (original), 2 = V2 (current default).
 * Override with -DBBCLAW_ST7789_147_VARIANT=1 if using V1 panel.
 */
#ifndef BBCLAW_ST7789_147_VARIANT
#define BBCLAW_ST7789_147_VARIANT 2
#endif

#define BBCLAW_ST7789_X_GAP         0
#define BBCLAW_ST7789_Y_GAP        34
#define BBCLAW_ST7789_PCLK_HZ      (20 * 1000 * 1000)
#define BBCLAW_ST7789_SWAP_XY       1
#define BBCLAW_ST7789_SWAP_BYTES    1

#if BBCLAW_ST7789_147_VARIANT == 1
#define BBCLAW_ST7789_MIRROR_X      1
#define BBCLAW_ST7789_MIRROR_Y      0
#define BBCLAW_ST7789_INVERT_COLOR  1
#define BBCLAW_ST7789_RGB_ORDER_BGR 1
#else
#define BBCLAW_ST7789_MIRROR_X      0
#define BBCLAW_ST7789_MIRROR_Y      0
#define BBCLAW_ST7789_INVERT_COLOR  0
#define BBCLAW_ST7789_RGB_ORDER_BGR 0
#endif

/* ── PA enable (not wired on breadboard) ── */
#define BBCLAW_PA_EN_GPIO      -1
