/**
 * BBClaw board config: 嘉立创·实战派 ESP32-S3（lichuang-szp）
 *
 * 硬件参考（引脚真相来源 = xiaozhi-esp32 main/boards/lichuang-dev/）：
 *   design/boards/lichuang-szp-esp32s3.md
 *
 * ESP32-S3-WROOM-1 N16R8（16MB flash + 8MB Octal PSRAM）+ 2.0" SPI ST7789
 * 320x240 + FT6336 触摸 + ES8311 codec（DAC）+ ES7210 ADC（mic）+
 * PCA9557 IO 扩展（LCD_CS / PA_EN / 摄像头 PWDN）。摄像头 OV2640 本阶段不适配。
 */
#pragma once

/* ── Audio: ES8311 放音 + ES7210 收音（同 I2S，与手表板同架构） ── */
#define BBCLAW_AUDIO_INPUT_SOURCE "es8311"
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

#define BBCLAW_AUDIO_I2S_MCK_GPIO 38
#define BBCLAW_AUDIO_I2S_BCK_GPIO 14
#define BBCLAW_AUDIO_I2S_WS_GPIO  13
#define BBCLAW_AUDIO_I2S_DO_GPIO  45  /* ESP TX → ES8311 DAC */
#define BBCLAW_AUDIO_I2S_DI_GPIO  12  /* ES7210 ADC → ESP RX */

/* 双 mic 走独立 ES7210 ADC（ES8311 只管 DAC），init 内自动探测 0x40-0x43 */
#define BBCLAW_ES7210_ENABLE 1

/* I2C 总线（ES8311 / ES7210 / PCA9557 / FT6336 共享） */
#define BBCLAW_ES8311_I2C_PORT     0
#define BBCLAW_ES8311_I2C_SDA_GPIO 1
#define BBCLAW_ES8311_I2C_SCL_GPIO 2
#define BBCLAW_ES8311_I2C_ADDR     0x18

/* ── PCA9557 IO 扩展（0x19）: bit0=LCD_CS(低=选通) bit1=PA_EN(高=开) ── */
#define BBCLAW_PCA9557_ENABLE      1
#define BBCLAW_PCA9557_I2C_ADDR    0x19
#define BBCLAW_PCA9557_LCD_CS_BIT  0
#define BBCLAW_PCA9557_PA_EN_BIT   1

/* 功放经 PCA9557，非直连 GPIO；GPIO1 是本板 I2C SDA，必须覆盖全局默认 SPK_SW=1 */
#define BBCLAW_PA_EN_GPIO        -1
#define BBCLAW_SPEAKER_SW_GPIO   -1
#define BBCLAW_PA_EN_PROBE_GPIO1 (-1)

/* ── OTA：独立平台名，云端无此平台 release → 不会被推 bbclaw 正式板固件 ── */
#define BBCLAW_OTA_PLATFORM "esp32s3-lichuang-szp"

/* ── PTT: BOOT 键（板上唯一用户键） ── */
#define BBCLAW_PTT_GPIO         0
#define BBCLAW_PTT_ACTIVE_LEVEL 0
#define BBCLAW_PTT_PULL_UP      1

/* ── Navigation: 无实体导航键；触摸 indev + 手势（右滑 BACK / 左滑 LEFT） ── */
#define BBCLAW_NAV_ENABLE 0

/* FT6336 触控（FT5x06 协议兼容，I2C 0x38；INT/RST 未接 GPIO） */
#define BBCLAW_TOUCH_FT5X06_ENABLE 1
#define BBCLAW_TOUCH_RST_GPIO -1
#define BBCLAW_TOUCH_INT_GPIO -1

/* ── Motor / 电量 / 状态灯：本板无此硬件（GPIO48 是 WS2812，bb_led 不支持，暂关） ── */
#define BBCLAW_MOTOR_ENABLE  0
#define BBCLAW_MOTOR_GPIO    -1
#define BBCLAW_POWER_ENABLE  0
#define BBCLAW_POWER_ADC_GPIO -1
#define BBCLAW_STATUS_LED_ENABLE 0

/* ── Display: SPI ST7789 2.0" 320x240 横屏 ── */
#define BBCLAW_DISPLAY_BUS_SPI   1
#define BBCLAW_DISPLAY_BUS_I80   0

#define BBCLAW_ST7789_HOST       2
#define BBCLAW_ST7789_SCLK_GPIO 41
#define BBCLAW_ST7789_MOSI_GPIO 40
#define BBCLAW_ST7789_DC_GPIO   39
#define BBCLAW_ST7789_RST_GPIO  -1  /* 未接 */
#define BBCLAW_ST7789_CS_GPIO   -1  /* CS 在 PCA9557 bit0，init 时拉低常选通 */
#define BBCLAW_ST7789_BL_GPIO   42
/* 背光高有效(真机实测:驱到 0 全黑、驱到 1 亮;xiaozhi 的 OUTPUT_INVERT 标记
 * 语义与直觉相反,勿照抄) */
#define BBCLAW_ST7789_BL_ACTIVE_LEVEL 1

#define BBCLAW_ST7789_WIDTH    320
#define BBCLAW_ST7789_HEIGHT   240
#define BBCLAW_ST7789_X_GAP    0
#define BBCLAW_ST7789_Y_GAP    0

/* 面板原生 240x320 竖屏，横屏 = swap_xy + mirror_x（与 xiaozhi lichuang-dev 一致） */
#define BBCLAW_ST7789_SWAP_XY       1
#define BBCLAW_ST7789_MIRROR_X      1
#define BBCLAW_ST7789_MIRROR_Y      0
#define BBCLAW_ST7789_INVERT_COLOR  1
#define BBCLAW_ST7789_RGB_ORDER_BGR 0
#define BBCLAW_ST7789_SWAP_BYTES    0
/* xiaozhi 用 80MHz；首次 bring-up 取项目惯用保守值，花屏再降 20MHz */
#define BBCLAW_ST7789_PCLK_HZ      (40 * 1000 * 1000)
/* 本板面板要 SPI mode 2（xiaozhi lichuang-dev 实证；mode 0 时命令收不进=黑屏） */
#define BBCLAW_ST7789_SPI_MODE     2

/* 息屏 = 关背光 GPIO42；无 IMU，消息唤醒 + 触摸/按键唤醒 */
#define BBCLAW_DISPLAY_BRIGHTNESS_CONTROL 1

/* LVGL 内部 DMA 缓冲减半(320*20*2=12.8KB):默认 40 行时 WiFi 起来后内部堆
 * largest 不足,cloud_saas TLS 握手 mbedtls alloc 失败(-0x7F00,真机踩过) */
#define BBCLAW_LVGL_BUFF_LINES 20
