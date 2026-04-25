/**
 * BBClaw board config: custom BBClaw PCB (U7 ESP32-S3, schematic rev aligned 2026-04)
 *
 * LCD: IO9–14 连续布线（SCL/SDA/RES/DC/CS/BLK）。
 * 音频：I2S BCK/WS/DO 仍为 16/15/17；原理图 IO18 未接麦克风，INMP441 SD 走 GPIO20。
 * IO2 未接 MCLK，INMP441 路径不使用 MCLK 输出。
 * 状态灯 U6：可寻址 RGB（DIN），net「RGB1」→ IO5；单线协议，非三线 PWM。DOUT 级联脚可悬空。
 *   当前 bb_led 仅支持共阴三线 PWM，故默认关闭状态灯，待 RMT/led_strip 接 GPIO5 后再开。
 */
#pragma once

/*
 * Audio: 外接 INMP441 + MAX98357A，无 ES8311 codec。
 *
 * 以模块丝印名为导向的接线表：
 *
 *   INMP441 丝印   →  ESP GPIO  →  作用（I2S）       →  等价宏
 *     SCK          →  GPIO16   →  BCLK（共享）           →  BBCLAW_MIC_SCK_GPIO  / BBCLAW_AUDIO_I2S_BCK_GPIO
 *     WS           →  GPIO15   →  WS/LRCK（共享）        →  BBCLAW_MIC_WS_GPIO   / BBCLAW_AUDIO_I2S_WS_GPIO
 *     SD           →  GPIO18   →  mic → ESP RX            →  BBCLAW_MIC_SD_GPIO   / BBCLAW_AUDIO_I2S_DI_GPIO
 *     VDD/GND/LR   →  3V3/GND/任选  （非 GPIO）
 *
 *   MAX98357A 丝印 →  ESP GPIO  →  作用（I2S）       →  等价宏
 *     BCLK         →  GPIO16   →  与麦克风 SCK 共线
 *     LRC          →  GPIO15   →  与麦克风 WS  共线
 *     DIN          →  GPIO17   →  ESP TX → amp           →  BBCLAW_SPK_DIN_GPIO  / BBCLAW_AUDIO_I2S_DO_GPIO
 *     SD           →  GPIO4    →  shutdown/使能（默认低=静音）  →  BBCLAW_SPK_SD_GPIO   / BBCLAW_SPEAKER_SW_GPIO
 *     VIN/GND      →  5V/GND   （非 GPIO）
 *
 *   MCLK（IO2）未引出，INMP441 不需要主时钟。
 */
#define BBCLAW_AUDIO_INPUT_SOURCE "inmp441"
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

#define BBCLAW_AUDIO_I2S_BCK_GPIO 16  /* mic SCK / spk BCLK (shared) */
#define BBCLAW_AUDIO_I2S_WS_GPIO  15  /* mic WS  / spk LRC  (shared) */
#define BBCLAW_AUDIO_I2S_DO_GPIO  17  /* ESP TX → MAX98357A DIN */
#define BBCLAW_AUDIO_I2S_DI_GPIO  18  /* INMP441 SD → ESP RX */
/** IO2 未接 MCLK；inmp441 路径不输出 MCLK */
#define BBCLAW_AUDIO_I2S_MCK_GPIO (-1)

/* ── PTT: U5 = GT-TC072A-H060-L1 capacitive touch sensor ──
 * "-L" variant: output LOW when touched, HIGH when idle.
 * Push-pull output, internal pull-up keeps GPIO HIGH at idle. */
#define BBCLAW_PTT_GPIO         7
#define BBCLAW_PTT_ACTIVE_LEVEL 0
#define BBCLAW_PTT_PULL_UP      1

/* ── Navigation wheel (rotary encoder + push) ── */
#define BBCLAW_NAV_ENABLE            1
#define BBCLAW_NAV_ENC_A_GPIO        6
#define BBCLAW_NAV_ENC_B_GPIO        8
#define BBCLAW_NAV_KEY_GPIO          1
#define BBCLAW_NAV_PULL_UP           1
#define BBCLAW_NAV_KEY_ACTIVE_LEVEL  0

/* ── Motor ── */
#define BBCLAW_MOTOR_GPIO      21
#define BBCLAW_MOTOR_ENABLE    1

/* ── Battery / power sensing ── */
#define BBCLAW_POWER_ENABLE            1
#define BBCLAW_POWER_ADC_GPIO          3
#define BBCLAW_POWER_ADC_RTOP_OHM      100000
#define BBCLAW_POWER_ADC_RBOT_OHM      100000
#define BBCLAW_POWER_BATTERY_FULL_MV   4200
#define BBCLAW_POWER_BATTERY_EMPTY_MV  3300

/* ── Status LED: U6 = WS2812 可寻址 RGB，单线 DIN = GPIO5（RGB1）── */
#define BBCLAW_STATUS_LED_ENABLE 1
#define BBCLAW_STATUS_LED_KIND_RGB_MODULE 0
#define BBCLAW_STATUS_LED_R_GPIO 5
#define BBCLAW_STATUS_LED_Y_GPIO (-1)
#define BBCLAW_STATUS_LED_G_GPIO (-1)

/* ── Display: SPI ST7789 1.47" 172x320 ── */
#define BBCLAW_DISPLAY_BUS_SPI   1
#define BBCLAW_DISPLAY_BUS_I80   0

#define BBCLAW_ST7789_HOST       2
#define BBCLAW_ST7789_SCLK_GPIO  9
#define BBCLAW_ST7789_MOSI_GPIO 10
#define BBCLAW_ST7789_RST_GPIO  11
#define BBCLAW_ST7789_DC_GPIO   12
#define BBCLAW_ST7789_CS_GPIO   13
#define BBCLAW_ST7789_BL_GPIO   14
#define BBCLAW_ST7789_WIDTH    320
#define BBCLAW_ST7789_HEIGHT   172

#ifndef BBCLAW_ST7789_147_VARIANT
#define BBCLAW_ST7789_147_VARIANT 2
#endif

#ifndef BBCLAW_DISPLAY_ROTATION_DEG
#define BBCLAW_DISPLAY_ROTATION_DEG 0
#endif

#if BBCLAW_DISPLAY_ROTATION_DEG != 0 && BBCLAW_DISPLAY_ROTATION_DEG != 90 && BBCLAW_DISPLAY_ROTATION_DEG != 180 && \
    BBCLAW_DISPLAY_ROTATION_DEG != 270
#error BBCLAW_DISPLAY_ROTATION_DEG must be 0, 90, 180, or 270
#endif

#define BBCLAW_ST7789_X_GAP         0
#define BBCLAW_ST7789_Y_GAP        34
#define BBCLAW_ST7789_PCLK_HZ      (20 * 1000 * 1000)
#define BBCLAW_ST7789_SWAP_XY       1
#define BBCLAW_ST7789_SWAP_BYTES    1

#if BBCLAW_ST7789_147_VARIANT == 2
#define BBCLAW_ST7789_INVERT_COLOR  1
#define BBCLAW_ST7789_RGB_ORDER_BGR 1
#if BBCLAW_DISPLAY_ROTATION_DEG == 0
#define BBCLAW_ST7789_MIRROR_X 1
#define BBCLAW_ST7789_MIRROR_Y 0
#elif BBCLAW_DISPLAY_ROTATION_DEG == 90
#define BBCLAW_ST7789_MIRROR_X 0
#define BBCLAW_ST7789_MIRROR_Y 0
#elif BBCLAW_DISPLAY_ROTATION_DEG == 180
#define BBCLAW_ST7789_MIRROR_X 0
#define BBCLAW_ST7789_MIRROR_Y 1
#else /* 270 */
#define BBCLAW_ST7789_MIRROR_X 1
#define BBCLAW_ST7789_MIRROR_Y 1
#endif
#else /* V1 */
#define BBCLAW_ST7789_INVERT_COLOR  0
#define BBCLAW_ST7789_RGB_ORDER_BGR 0
#if BBCLAW_DISPLAY_ROTATION_DEG == 0
#define BBCLAW_ST7789_MIRROR_X 0
#define BBCLAW_ST7789_MIRROR_Y 0
#elif BBCLAW_DISPLAY_ROTATION_DEG == 90
#define BBCLAW_ST7789_MIRROR_X 1
#define BBCLAW_ST7789_MIRROR_Y 0
#elif BBCLAW_DISPLAY_ROTATION_DEG == 180
#define BBCLAW_ST7789_MIRROR_X 1
#define BBCLAW_ST7789_MIRROR_Y 1
#else /* 270 */
#define BBCLAW_ST7789_MIRROR_X 0
#define BBCLAW_ST7789_MIRROR_Y 1
#endif
#endif

/* ── PA enable / speaker SD ENable ──
 * MAX98357A 丝印 SD 引脚（shutdown / mode）走 GPIO4，LOW = 静音/关功放。
 * 等价别名：BBCLAW_SPK_SD_GPIO。 */
#define BBCLAW_PA_EN_GPIO      -1
#define BBCLAW_SPEAKER_SW_GPIO  4
/** Default probe GPIO1 was 13; on this PCB GPIO13 is LCD CS */
#define BBCLAW_PA_EN_PROBE_GPIO1 (-1)
