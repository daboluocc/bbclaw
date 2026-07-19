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

/* ── PCA9557 IO 扩展（0x19）: bit0=LCD_CS(低=选通) bit1=PA_EN(高=开) bit2=CAM_PWDN ── */
#define BBCLAW_PCA9557_ENABLE      1
#define BBCLAW_PCA9557_I2C_ADDR    0x19
#define BBCLAW_PCA9557_LCD_CS_BIT  0
#define BBCLAW_PCA9557_PA_EN_BIT   1
/* 摄像头 PWDN 挂 PCA9557 bit2（高=掉电，低=工作）。enable camera 时该位配为输出。 */
#define BBCLAW_PCA9557_CAM_PWDN_BIT 2

/* ── Camera: OV2640 DVP（ADR-049 Phase 1 bring-up） ──
 * 引脚真相来源 = xiaozhi lichuang-dev config.h（同板同源，已验证与显示/音频共存）。
 * GPIO 冲突核对（2026-07-19）：下列 12 根 DVP 独占线与现用引脚零重叠——
 *   现用: I2S 38/14/13/45/12, I2C 1/2, PTT 0, 显示 41/40/39/42, WS2812 48。
 * SCCB(配置总线)= 复用已初始化的 port0 I2C(GPIO1/2)；PWDN 走 PCA9557 bit2。 */
#define BBCLAW_CAMERA_ENABLE     1
#define BBCLAW_CAMERA_SCCB_PORT  0   /* 复用 ES8311/PCA9557 的 port0 I2C 总线 */
#define BBCLAW_CAMERA_PIN_XCLK   5
#define BBCLAW_CAMERA_PIN_PCLK   7
#define BBCLAW_CAMERA_PIN_VSYNC  3
#define BBCLAW_CAMERA_PIN_HREF   46
#define BBCLAW_CAMERA_PIN_SIOC   2   /* SCCB SCL（= I2C SCL，复用） */
#define BBCLAW_CAMERA_PIN_SIOD   1   /* SCCB SDA（= I2C SDA，复用；驱动侧填 -1 走总线复用） */
#define BBCLAW_CAMERA_PIN_D0     16
#define BBCLAW_CAMERA_PIN_D1     18
#define BBCLAW_CAMERA_PIN_D2     8
#define BBCLAW_CAMERA_PIN_D3     17
#define BBCLAW_CAMERA_PIN_D4     15
#define BBCLAW_CAMERA_PIN_D5     6
#define BBCLAW_CAMERA_PIN_D6     4
#define BBCLAW_CAMERA_PIN_D7     9
#define BBCLAW_CAMERA_XCLK_HZ    20000000
/* frame2jpg 软编质量 0-63（越小越清越大）。10 → VGA 约 30-50KB，base64 后远在 1MiB 内。 */
#define BBCLAW_CAMERA_JPEG_QUALITY 10
/* Phase 1 自测：boot 时拍一帧 JPEG 打日志（size + FFD8 magic + 分辨率）。生产关。 */
#define BBCLAW_CAMERA_SELFTEST   1
/* Phase 1 端到端测试：cloud 连上后拍一张发 image.capture 给 adapter（验证上行链路）。生产关。 */
#define BBCLAW_CAMERA_TEST_UPLOAD 1

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

/* FT6336 触控（FT5x06 协议兼容，I2C 0x38；INT/RST 未接 GPIO）。
 * 触摸报的是面板原生竖屏坐标,横屏显示需 swap+mirror 与 MADCTL 同步
 * (xiaozhi lichuang-dev 同参数)。 */
#define BBCLAW_TOUCH_FT5X06_ENABLE 1
#define BBCLAW_TOUCH_RST_GPIO -1
#define BBCLAW_TOUCH_INT_GPIO -1
#define BBCLAW_TOUCH_SWAP_XY  1
#define BBCLAW_TOUCH_MIRROR_X 1
#define BBCLAW_TOUCH_MIRROR_Y 0

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
/* 背光必须 PWM(2026-07-19 排障终局结论):升压电路要开关信号,恒定电平 0/1 都
 * 不亮。5kHz/50% 实测点亮。命令/参数/像素链路当时全正常(RDDPM=9C/MADCTL=60/
 * RAMRD 回读像素实证),黑屏唯一根因就是这里。 */
#define BBCLAW_ST7789_BL_PWM 1

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
/* 与 bbclaw 板一致:最低亮度常显 Z··· 呼吸待机页,自动超时不关背光/DISPOFF。 */
#define BBCLAW_SLEEP_MANAGER_AMBIENT_STANDBY 1

/* LVGL 内部 DMA 缓冲减半(320*20*2=12.8KB):默认 40 行时 WiFi 起来后内部堆
 * largest 不足,cloud_saas TLS 握手 mbedtls alloc 失败(-0x7F00,真机踩过) */
#define BBCLAW_LVGL_BUFF_LINES 20
