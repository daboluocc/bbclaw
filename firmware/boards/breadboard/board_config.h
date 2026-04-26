/**
 * BBClaw board config: breadboard
 *
 * Wired to match BBClaw PCB pin assignments (schematic rev 2026-04).
 * Differences from BBClaw PCB are documented per section below.
 */
#pragma once

/*
 * Audio: INMP441 mic + MAX98357A amp (same as BBClaw PCB).
 * DI_GPIO/MCK_GPIO kept at breadboard wire positions.
 */
#define BBCLAW_AUDIO_INPUT_SOURCE "inmp441"
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

#define BBCLAW_AUDIO_I2S_BCK_GPIO 16
#define BBCLAW_AUDIO_I2S_WS_GPIO  15
#define BBCLAW_AUDIO_I2S_DO_GPIO  17
/** Breadboard: INMP441 SD wired to GPIO18 (BBClaw PCB uses GPIO20) */
#define BBCLAW_AUDIO_I2S_DI_GPIO  18
/** Breadboard: MCLK wired to GPIO2 (BBClaw PCB leaves IO2 unconnected) */
#define BBCLAW_AUDIO_I2S_MCK_GPIO  2

/*
 * PTT: plain push-button on breadboard — one end to GPIO7, the other to GND.
 * Idle pulled HIGH by internal pull-up; pressed = LOW (matches BBClaw PCB NAV key on GPIO1).
 */
#define BBCLAW_PTT_GPIO         7
#define BBCLAW_PTT_ACTIVE_LEVEL 0
#define BBCLAW_PTT_PULL_UP      1

/*
 * Navigation: Flipper Zero 6-button layout (Phase 5 / Option B — full events).
 * Now wired to a 5-way joystick + 2 side keys integrated module (2026-04-27);
 * the GPIO mapping is identical to the previous 6-discrete-button setup, so
 * firmware sees no difference.
 *
 *   GPIO 6  UP    → BB_NAV_EVENT_UP    (press edge, picker scroll up)
 *   GPIO 8  DOWN  → BB_NAV_EVENT_DOWN  (press edge, picker scroll down)
 *   GPIO 38 LEFT  → BB_NAV_EVENT_LEFT  (press edge — Phase 5 cycle driver -1)
 *   GPIO 39 RIGHT → BB_NAV_EVENT_RIGHT (press edge — Phase 5 cycle driver +1)
 *   GPIO 1  OK    → BB_NAV_EVENT_OK    (release edge, like the legacy KEY)
 *   GPIO 47 BACK  → BB_NAV_EVENT_BACK  (press edge — explicit "exit" key)
 *
 * Was on GPIO 0 (BOOT strap) historically — moved to GPIO 47 on 2026-04-27
 * because GPIO 0 must be HIGH at boot for the chip to enter normal run mode,
 * and an accidental BACK press during USB plug-in would drop the chip into
 * download mode. GPIO 47 is a free, non-strapping pin.
 *
 * Press = active LOW with internal pull-up. BBCLAW_NAV_FLIPPER_6BUTTON=1
 * routes bb_nav_input.c through the 6-button branch; the legacy
 * BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC and BBCLAW_NAV_ENC_A/B/KEY_GPIO macros
 * default sensibly via bb_config.h's -1 defaults.
 */
#define BBCLAW_NAV_ENABLE                   1
#define BBCLAW_NAV_FLIPPER_6BUTTON          1
#define BBCLAW_NAV_BTN_UP_GPIO              6
#define BBCLAW_NAV_BTN_DOWN_GPIO            8
#define BBCLAW_NAV_BTN_OK_GPIO              1
#define BBCLAW_NAV_BTN_BACK_GPIO            47
#define BBCLAW_NAV_BTN_LEFT_GPIO            38
#define BBCLAW_NAV_BTN_RIGHT_GPIO           39
#define BBCLAW_NAV_PULL_UP                  1
#define BBCLAW_NAV_KEY_ACTIVE_LEVEL         0


/* ── Motor ── */
#define BBCLAW_MOTOR_GPIO      21
#define BBCLAW_MOTOR_ENABLE    1

/*
 * Battery / power sensing: no ADC divider on breadboard.
 */
#define BBCLAW_POWER_ENABLE    0

/*
 * Status LED: discrete R/Y/G on breadboard (BBClaw PCB uses WS2812 on GPIO5).
 */
#define BBCLAW_STATUS_LED_ENABLE          1
#define BBCLAW_STATUS_LED_KIND_RGB_MODULE 1
#define BBCLAW_STATUS_LED_R_GPIO          2
#define BBCLAW_STATUS_LED_Y_GPIO          4
#define BBCLAW_STATUS_LED_G_GPIO          5

/* ── Display: SPI ST7789 1.47" 172x320, same GPIO as BBClaw PCB ── */
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

/*
 * Panel variant: 1 = V1 (original), 2 = V2 (current default).
 * Override with -DBBCLAW_ST7789_147_VARIANT=1 if using V1 panel.
 */
#ifndef BBCLAW_ST7789_147_VARIANT
#define BBCLAW_ST7789_147_VARIANT 2
#endif

/**
 * 屏幕安装角度（顺时针，相对该 variant 的默认朝向）。
 * 仅影响 esp_lcd swap_xy / mirror，不改变 WIDTH/HEIGHT 宏（LVGL 仍为 320x172）。
 * 若某档倒置或镜像不对，试相邻的 90° 档，或用编译参数覆盖单宏。
 */
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

/* ── PA enable / speaker sense: not wired on breadboard ── */
#define BBCLAW_PA_EN_GPIO        -1
#define BBCLAW_SPEAKER_SW_GPIO   -1
#define BBCLAW_PA_EN_PROBE_GPIO1 (-1)
