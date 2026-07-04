/**
 * BBClaw board config: Waveshare ESP32-S3-Touch-AMOLED-2.06（拓展板，ADR-040）
 *
 * 硬件参考（引脚真相来源 = 官方 BSP waveshare/esp32_s3_touch_amoled_2_06 v2.0.0）：
 *   design/boards/waveshare-esp32-s3-touch-amoled-2.06.md
 *
 * 2.06" AMOLED 410x502（CO5300, QSPI, SH8601 兼容指令集）+ FT3168 触摸 +
 * ES8311 codec + AXP2101 PMIC + QMI8658 + PCF85063，Octal PSRAM 8MB。
 *
 * 第一阶段适配范围（ADR-040 §5/§6）：显示 + ES8311 收放 + BOOT 键 PTT。
 * 触摸 / AXP2101 电量 / IMU / RTC / SD 卡为后续增量，本文件先禁用相关子系统。
 */
#pragma once

/* ── Audio: ES8311 codec（I2C 控制 + I2S duplex）——bb_audio 既有路径首次真机启用 ── */
#define BBCLAW_AUDIO_INPUT_SOURCE "es8311"
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

#define BBCLAW_AUDIO_I2S_MCK_GPIO 16  /* ES8311 需要 MCLK */
#define BBCLAW_AUDIO_I2S_BCK_GPIO 41
#define BBCLAW_AUDIO_I2S_WS_GPIO  45
#define BBCLAW_AUDIO_I2S_DO_GPIO  40  /* ESP TX → codec DAC */
#define BBCLAW_AUDIO_I2S_DI_GPIO  42  /* codec ADC → ESP RX */

/* ADC 数字音量对齐官方 mic 配置（es8311_microphone_config 写 0xC8 ≈ +4dB） */
#define BBCLAW_ES8311_ADC_VOLUME 0xC8

/* I2C 总线（ES8311 / FT3168 / AXP2101 / QMI8658 / PCF85063 共享） */
#define BBCLAW_ES8311_I2C_PORT     0
#define BBCLAW_ES8311_I2C_SDA_GPIO 15
#define BBCLAW_ES8311_I2C_SCL_GPIO 14
#define BBCLAW_ES8311_I2C_ADDR     0x18

/* AXP2101（0x34，同 I2C 总线）：ALDO1=MIC 供电轨必须使能 + 充电参数 */
#define BBCLAW_AXP2101_MINIMAL_INIT 1

/* 功放使能：GPIO46 高有效（官方 BSP pa_pin） */
#define BBCLAW_PA_EN_GPIO          46
#define BBCLAW_PA_EN_ACTIVE_LEVEL  1
#define BBCLAW_SPEAKER_SW_GPIO     -1
#define BBCLAW_PA_EN_PROBE_GPIO1   (-1)

/* ── OTA：独立平台名，云端无此平台的 release → 不会被推 bbclaw 正式板固件 ── */
#define BBCLAW_OTA_PLATFORM "esp32s3-ws-amoled-206"

/* ── PTT: BOOT 键（板上无侧键/滚轮） ── */
#define BBCLAW_PTT_GPIO         0
#define BBCLAW_PTT_ACTIVE_LEVEL 0
#define BBCLAW_PTT_PULL_UP      1

/* ── Navigation: 无实体导航键；触摸 indev 为第二阶段（ADR-040 §5） ── */
#define BBCLAW_NAV_ENABLE 0

/* ── Motor / 电量 / 状态灯：本板无此硬件 ──
 * 电量在 AXP2101 里（I2C），ADC 分压路径不存在；PMU 驱动属第二阶段。 */
#define BBCLAW_MOTOR_ENABLE  0
#define BBCLAW_MOTOR_GPIO    -1
#define BBCLAW_POWER_ENABLE  0
#define BBCLAW_POWER_ADC_GPIO -1
#define BBCLAW_STATUS_LED_ENABLE 0

/* ── Display: QSPI AMOLED CO5300 410x502（SH8601 兼容驱动） ── */
#define BBCLAW_DISPLAY_BUS_SPI   0
#define BBCLAW_DISPLAY_BUS_I80   0
#define BBCLAW_DISPLAY_BUS_QSPI  1

#define BBCLAW_QSPI_HOST      2   /* SPI2_HOST */
#define BBCLAW_QSPI_SCK_GPIO  11
#define BBCLAW_QSPI_CS_GPIO   12
#define BBCLAW_QSPI_D0_GPIO    4
#define BBCLAW_QSPI_D1_GPIO    5
#define BBCLAW_QSPI_D2_GPIO    6
#define BBCLAW_QSPI_D3_GPIO    7
#define BBCLAW_QSPI_RST_GPIO   8

/* 面板通用参数沿用 ST7789 命名的宏（显示栈 DISP_W/H 等都挂在这组宏上） */
#define BBCLAW_ST7789_WIDTH   410
#define BBCLAW_ST7789_HEIGHT  502
/* AMOLED 无背光 GPIO：亮度走 CO5300 0x51 命令，init cmds 末尾已置 0xFF 满亮 */
#define BBCLAW_ST7789_BL_GPIO  -1
#define BBCLAW_ST7789_RST_GPIO BBCLAW_QSPI_RST_GPIO
/* CO5300 列地址从 0x16 起（面板 X gap = 22px） */
#define BBCLAW_ST7789_X_GAP    0x16
#define BBCLAW_ST7789_Y_GAP    0
#define BBCLAW_ST7789_SWAP_XY       0
#define BBCLAW_ST7789_MIRROR_X      0
#define BBCLAW_ST7789_MIRROR_Y      0
#define BBCLAW_ST7789_INVERT_COLOR  0
#define BBCLAW_ST7789_RGB_ORDER_BGR 0
#define BBCLAW_ST7789_SWAP_BYTES    0

/* CO5300 要求刷新区域 2px 对齐（起点取偶、终点取奇），见硬件参考文档 §怪癖2 */
#define BBCLAW_DISPLAY_PIXEL_ALIGN  2

/* LVGL 缓冲：必须内部 DMA 单缓冲（410*40*2 ≈ 32.8KB，display init 时一次分配）。
 * 不能学官方 BSP 放 PSRAM：esp_lcd SPI 对非 DMA 缓冲每次刷屏都会 malloc 同尺寸的
 * 内部反弹缓冲，softAP 起来后内部 DMA largest(40960) < flush 块(41000) → flush 失败
 * → lvgl_port 永远等不到 flush_ready → LVGL 持锁整机冻结（真机踩过）。
 * esp_lvgl_port 的 trans_size 分块方案仅 lvgl8 后端实现，LVGL9 不可用。 */
#define BBCLAW_LVGL_BUFF_LINES   40
#define BBCLAW_LVGL_BUFF_SPIRAM  0
#define BBCLAW_LVGL_BUFF_DOUBLE  0
