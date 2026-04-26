#pragma once

#include "sdkconfig.h"

/* ── Board selection: include board-specific pin map and hardware config ── */
#if defined(CONFIG_BBCLAW_BOARD_ATK_DNESP32S3_BOX)
#include "../boards/atk-dnesp32s3-box/board_config.h"
#elif defined(CONFIG_BBCLAW_BOARD_BBCLAW)
#include "../boards/bbclaw/board_config.h"
#elif defined(CONFIG_BBCLAW_BOARD_BREADBOARD) || !defined(BBCLAW_DISPLAY_BUS_SPI)
#include "../boards/breadboard/board_config.h"
#endif

/* ── Default bus flags (boards that predate the bus flag) ── */
#ifndef BBCLAW_DISPLAY_BUS_SPI
#define BBCLAW_DISPLAY_BUS_SPI 1
#endif
#ifndef BBCLAW_DISPLAY_BUS_I80
#define BBCLAW_DISPLAY_BUS_I80 0
#endif

#ifndef BBCLAW_XL9555_ENABLE
#define BBCLAW_XL9555_ENABLE 0
#endif

#ifndef BBCLAW_NODE_ID
#define BBCLAW_NODE_ID "bbclaw-esp32s3"
#endif

#ifndef BBCLAW_GATEWAY_URL
#define BBCLAW_GATEWAY_URL "ws://192.168.1.10:19089/gateway"
#endif

#ifndef BBCLAW_PAIRING_TOKEN
#define BBCLAW_PAIRING_TOKEN ""
#endif

#ifndef BBCLAW_WIFI_SSID
#ifdef CONFIG_BBCLAW_WIFI_SSID
#define BBCLAW_WIFI_SSID CONFIG_BBCLAW_WIFI_SSID
#else
#define BBCLAW_WIFI_SSID ""
#endif
#endif

#ifndef BBCLAW_WIFI_PASSWORD
#ifdef CONFIG_BBCLAW_WIFI_PASSWORD
#define BBCLAW_WIFI_PASSWORD CONFIG_BBCLAW_WIFI_PASSWORD
#else
#define BBCLAW_WIFI_PASSWORD ""
#endif
#endif

#ifndef BBCLAW_WIFI_STA_MAX_RETRY
#define BBCLAW_WIFI_STA_MAX_RETRY 3
#endif

#ifndef BBCLAW_WIFI_STA_CONNECT_TIMEOUT_MS
#define BBCLAW_WIFI_STA_CONNECT_TIMEOUT_MS 30000
#endif

#ifndef BBCLAW_WIFI_NVS_NAMESPACE
#define BBCLAW_WIFI_NVS_NAMESPACE "bbwifi"
#endif

#ifndef BBCLAW_WIFI_NVS_KEY_SSID
#define BBCLAW_WIFI_NVS_KEY_SSID "sta_ssid"
#endif

#ifndef BBCLAW_WIFI_NVS_KEY_PASSWORD
#define BBCLAW_WIFI_NVS_KEY_PASSWORD "sta_pass"
#endif

#ifndef BBCLAW_WIFI_MAX_SAVED
#define BBCLAW_WIFI_MAX_SAVED 4
#endif

#ifndef BBCLAW_WIFI_AP_SSID_PREFIX
#define BBCLAW_WIFI_AP_SSID_PREFIX "BBClaw-Setup"
#endif

#ifndef BBCLAW_WIFI_AP_PASSWORD
#define BBCLAW_WIFI_AP_PASSWORD "bbclaw1234"
#endif

#ifndef BBCLAW_WIFI_AP_CHANNEL
#define BBCLAW_WIFI_AP_CHANNEL 6
#endif

#ifndef BBCLAW_WIFI_AP_MAX_CONNECTIONS
#define BBCLAW_WIFI_AP_MAX_CONNECTIONS 4
#endif

#ifndef BBCLAW_ADAPTER_BASE_URL
#ifdef CONFIG_BBCLAW_ADAPTER_BASE_URL
#define BBCLAW_ADAPTER_BASE_URL CONFIG_BBCLAW_ADAPTER_BASE_URL
#else
#define BBCLAW_ADAPTER_BASE_URL "http://192.168.10.26:18080"
#endif
#endif

#ifndef BBCLAW_TRANSPORT_PROFILE
#if defined(CONFIG_BBCLAW_TRANSPORT_PROFILE_CLOUD_SAAS)
#define BBCLAW_TRANSPORT_PROFILE "cloud_saas"
#else
#define BBCLAW_TRANSPORT_PROFILE "local_home"
#endif
#endif

#ifndef BBCLAW_CLOUD_BASE_URL
#ifdef CONFIG_BBCLAW_CLOUD_BASE_URL
#define BBCLAW_CLOUD_BASE_URL CONFIG_BBCLAW_CLOUD_BASE_URL
#else
#define BBCLAW_CLOUD_BASE_URL "http://bbclaw.daboluo.cc:38082"
#endif
#endif

#ifndef BBCLAW_CLOUD_AUDIO_STREAMING_READY
#define BBCLAW_CLOUD_AUDIO_STREAMING_READY 1
#endif

/** 为 1 时：每包 /v1/stream/chunk 打印 wall_span_ms / gap_prev_ms / http_ms，便于对照服务端 AUDIO_TOO_LONG */
#ifndef BBCLAW_ADAPTER_STREAM_CHUNK_DIAG
#define BBCLAW_ADAPTER_STREAM_CHUNK_DIAG 1
#endif

#ifndef BBCLAW_ENABLE_DISPLAY_PULL
#ifdef CONFIG_BBCLAW_ENABLE_DISPLAY_PULL
#define BBCLAW_ENABLE_DISPLAY_PULL 1
#else
#define BBCLAW_ENABLE_DISPLAY_PULL 0
#endif
#endif

/** 云上 deviceId：BBClaw-<固件版本>-<MAC 后三字节>（与 SoftAP「前缀-后缀」同源）；运行时见 bb_identity.c */
#ifndef BBCLAW_DEVICE_ID
const char *bbclaw_device_id(void);
#define BBCLAW_DEVICE_ID (bbclaw_device_id())
#endif

#ifndef BBCLAW_SESSION_KEY
const char *bbclaw_session_key(void);
#define BBCLAW_SESSION_KEY (bbclaw_session_key())
#endif

#ifndef BBCLAW_STREAM_CODEC
#define BBCLAW_STREAM_CODEC "opus"
#endif

#ifndef BBCLAW_LOCAL_LOOPBACK_ONLY
#define BBCLAW_LOCAL_LOOPBACK_ONLY 0
#endif

#ifndef BBCLAW_LOCAL_LOOPBACK_MAX_MS
#define BBCLAW_LOCAL_LOOPBACK_MAX_MS 8000
#endif

#ifndef BBCLAW_ENABLE_TTS_PLAYBACK
#ifdef CONFIG_BBCLAW_ENABLE_TTS_PLAYBACK
#define BBCLAW_ENABLE_TTS_PLAYBACK 1
#else
#define BBCLAW_ENABLE_TTS_PLAYBACK 0
#endif
#endif

#ifndef BBCLAW_TTS_SAMPLE_RATE
#define BBCLAW_TTS_SAMPLE_RATE 16000
#endif

#ifndef BBCLAW_TTS_CHANNELS
#define BBCLAW_TTS_CHANNELS 1
#endif

/** TTS 播放音量百分比（0-100），100=原始音量，50=减半 */
#ifndef BBCLAW_TTS_VOLUME_PCT
#define BBCLAW_TTS_VOLUME_PCT 50
#endif

#ifndef BBCLAW_SPK_TEST_ON_BOOT
#define BBCLAW_SPK_TEST_ON_BOOT 1
#endif

#ifndef BBCLAW_PA_EN_GPIO
#define BBCLAW_PA_EN_GPIO -1
#endif

#ifndef BBCLAW_PA_EN_ACTIVE_LEVEL
#define BBCLAW_PA_EN_ACTIVE_LEVEL 1
#endif

#ifndef BBCLAW_SPEAKER_SW_GPIO
#define BBCLAW_SPEAKER_SW_GPIO 1
#endif

#ifndef BBCLAW_SPEAKER_SW_ACTIVE_LEVEL
#define BBCLAW_SPEAKER_SW_ACTIVE_LEVEL 0
#endif

#ifndef BBCLAW_PA_EN_PROBE_ON_BOOT
#define BBCLAW_PA_EN_PROBE_ON_BOOT 0
#endif

#ifndef BBCLAW_PA_EN_PROBE_GPIO1
#define BBCLAW_PA_EN_PROBE_GPIO1 13
#endif

#ifndef BBCLAW_PA_EN_PROBE_GPIO2
#define BBCLAW_PA_EN_PROBE_GPIO2 -1
#endif

#ifndef BBCLAW_PA_EN_PROBE_GPIO3
#define BBCLAW_PA_EN_PROBE_GPIO3 -1
#endif

/**
 * PTT 只使用一个引脚（BBCLAW_PTT_GPIO）。默认“普通按键接地”接法：按下→LOW，内部上拉。
 * 若外接键为“按下接 3V3”，改为 ACTIVE_LEVEL=1 / PULL_UP=0（可参考 bb_button_test 日志）。
 */
#ifndef BBCLAW_PTT_GPIO
#define BBCLAW_PTT_GPIO 7
#endif

#ifndef BBCLAW_PTT_ACTIVE_LEVEL
#define BBCLAW_PTT_ACTIVE_LEVEL 0
#endif

#ifndef BBCLAW_PTT_PULL_UP
#define BBCLAW_PTT_PULL_UP 1
#endif

#ifndef BBCLAW_PTT_DEBOUNCE_MS
#define BBCLAW_PTT_DEBOUNCE_MS 30
#endif

#ifndef BBCLAW_NAV_ENABLE
#define BBCLAW_NAV_ENABLE 0
#endif

#ifndef BBCLAW_NAV_ENC_A_GPIO
#define BBCLAW_NAV_ENC_A_GPIO -1
#endif

#ifndef BBCLAW_NAV_ENC_B_GPIO
#define BBCLAW_NAV_ENC_B_GPIO -1
#endif

#ifndef BBCLAW_NAV_KEY_GPIO
#define BBCLAW_NAV_KEY_GPIO -1
#endif

#ifndef BBCLAW_NAV_PULL_UP
#define BBCLAW_NAV_PULL_UP 1
#endif

#ifndef BBCLAW_NAV_KEY_ACTIVE_LEVEL
#define BBCLAW_NAV_KEY_ACTIVE_LEVEL 0
#endif

#ifndef BBCLAW_NAV_POLL_MS
#define BBCLAW_NAV_POLL_MS 2
#endif

#ifndef BBCLAW_NAV_KEY_DEBOUNCE_MS
#define BBCLAW_NAV_KEY_DEBOUNCE_MS 20
#endif

#ifndef BBCLAW_NAV_LONG_PRESS_MS
#define BBCLAW_NAV_LONG_PRESS_MS 700
#endif

/**
 * 把 ENC_A / ENC_B 当作两个独立按键来处理（按下 A → ROTATE_CCW，按下 B → ROTATE_CW）。
 * 默认 0 = 走正交编码器解码（bbclaw 生产板的旋钮编码器）。
 * 面包板没有真编码器、用普通按键代替时改成 1。
 */
#ifndef BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
#define BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC 0
#endif

/* Flipper 6-button layout (Phase 5 / Option B: full dedicated events).
 * When BBCLAW_NAV_FLIPPER_6BUTTON=1, bb_nav_input.c reads UP/DOWN/
 * LEFT/RIGHT/OK/BACK as 6 individual edge-detected buttons.
 * Mutually exclusive with BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC.
 *
 * Event mapping (each button has a dedicated event; the legacy
 * ROTATE_CCW / ROTATE_CW / CLICK / LONG_PRESS names remain as aliases):
 *   UP    → BB_NAV_EVENT_UP    (press edge)
 *   DOWN  → BB_NAV_EVENT_DOWN  (press edge)
 *   LEFT  → BB_NAV_EVENT_LEFT  (press edge — Phase 5: cycle agent driver -1)
 *   RIGHT → BB_NAV_EVENT_RIGHT (press edge — Phase 5: cycle agent driver +1)
 *   OK    → BB_NAV_EVENT_OK    (release edge, like the legacy KEY)
 *   BACK  → BB_NAV_EVENT_BACK  (press edge — explicit "exit overlay" key)
 *
 * All 6 share BBCLAW_NAV_KEY_ACTIVE_LEVEL / BBCLAW_NAV_PULL_UP. Set the
 * GPIO macro to -1 to skip wiring an individual button. */
#ifndef BBCLAW_NAV_FLIPPER_6BUTTON
#define BBCLAW_NAV_FLIPPER_6BUTTON 0
#endif

#ifndef BBCLAW_NAV_BTN_UP_GPIO
#define BBCLAW_NAV_BTN_UP_GPIO -1
#endif

#ifndef BBCLAW_NAV_BTN_DOWN_GPIO
#define BBCLAW_NAV_BTN_DOWN_GPIO -1
#endif

#ifndef BBCLAW_NAV_BTN_LEFT_GPIO
#define BBCLAW_NAV_BTN_LEFT_GPIO -1
#endif

#ifndef BBCLAW_NAV_BTN_RIGHT_GPIO
#define BBCLAW_NAV_BTN_RIGHT_GPIO -1
#endif

#ifndef BBCLAW_NAV_BTN_OK_GPIO
#define BBCLAW_NAV_BTN_OK_GPIO -1
#endif

#ifndef BBCLAW_NAV_BTN_BACK_GPIO
#define BBCLAW_NAV_BTN_BACK_GPIO -1
#endif

#if BBCLAW_NAV_FLIPPER_6BUTTON && BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC
#error "BBCLAW_NAV_FLIPPER_6BUTTON and BBCLAW_NAV_BUTTONS_INSTEAD_OF_ENC are mutually exclusive"
#endif

/** 外接按键测试（仅调试用）：设为 -1 关闭；与 PTT 同脚时不要开，避免重复配置 GPIO */
#ifndef BBCLAW_BUTTON_TEST_GPIO
#define BBCLAW_BUTTON_TEST_GPIO -1
#endif
#ifndef BBCLAW_BUTTON_TEST_PULL_UP
#define BBCLAW_BUTTON_TEST_PULL_UP 0
#endif
#ifndef BBCLAW_BUTTON_TEST_INTERVAL_MS
#define BBCLAW_BUTTON_TEST_INTERVAL_MS 50
#endif

#ifndef BBCLAW_MOTOR_ENABLE
#define BBCLAW_MOTOR_ENABLE 1
#endif

#ifndef BBCLAW_MOTOR_GPIO
#define BBCLAW_MOTOR_GPIO 21
#endif

#ifndef BBCLAW_MOTOR_ACTIVE_LEVEL
#define BBCLAW_MOTOR_ACTIVE_LEVEL 1
#endif

#ifndef BBCLAW_POWER_ENABLE
#define BBCLAW_POWER_ENABLE 0
#endif

#ifndef BBCLAW_POWER_ADC_GPIO
#define BBCLAW_POWER_ADC_GPIO -1
#endif

#ifndef BBCLAW_POWER_ADC_RTOP_OHM
#define BBCLAW_POWER_ADC_RTOP_OHM 100000
#endif

#ifndef BBCLAW_POWER_ADC_RBOT_OHM
#define BBCLAW_POWER_ADC_RBOT_OHM 100000
#endif

#ifndef BBCLAW_POWER_BATTERY_FULL_MV
#define BBCLAW_POWER_BATTERY_FULL_MV 4200
#endif

#ifndef BBCLAW_POWER_BATTERY_EMPTY_MV
#define BBCLAW_POWER_BATTERY_EMPTY_MV 3300
#endif

#ifndef BBCLAW_POWER_LOW_PERCENT
#define BBCLAW_POWER_LOW_PERCENT 15
#endif

#ifndef BBCLAW_POWER_POLL_INTERVAL_MS
#define BBCLAW_POWER_POLL_INTERVAL_MS 5000
#endif

/** PTT 按下：偏心马达需 ~50ms+ 才易感知，过短会像“没震” */
#ifndef BBCLAW_MOTOR_PULSE_SHORT_MS
#define BBCLAW_MOTOR_PULSE_SHORT_MS 500
#endif

/** PTT 松开：比按下略短、略轻，形成“按下—松开”一对触觉 */
#ifndef BBCLAW_MOTOR_PULSE_RELEASE_MS
#define BBCLAW_MOTOR_PULSE_RELEASE_MS 400
#endif

#ifndef BBCLAW_MOTOR_PULSE_LONG_MS
#define BBCLAW_MOTOR_PULSE_LONG_MS 400
#endif

#ifndef BBCLAW_MOTOR_PULSE_GAP_MS
#define BBCLAW_MOTOR_PULSE_GAP_MS 100
#endif

#ifndef BBCLAW_STATUS_LED_ENABLE
#define BBCLAW_STATUS_LED_ENABLE 1
#endif

#ifndef BBCLAW_STATUS_LED_KIND_RGB_MODULE
#define BBCLAW_STATUS_LED_KIND_RGB_MODULE 0
#endif

#ifndef BBCLAW_STATUS_LED_R_GPIO
#define BBCLAW_STATUS_LED_R_GPIO 2
#endif

#ifndef BBCLAW_STATUS_LED_Y_GPIO
#define BBCLAW_STATUS_LED_Y_GPIO 4
#endif

#ifndef BBCLAW_STATUS_LED_G_GPIO
#define BBCLAW_STATUS_LED_G_GPIO 5
#endif

#ifndef BBCLAW_STATUS_LED_RGB_G_GPIO
#define BBCLAW_STATUS_LED_RGB_G_GPIO 4
#endif

#ifndef BBCLAW_STATUS_LED_RGB_B_GPIO
#define BBCLAW_STATUS_LED_RGB_B_GPIO 5
#endif

#ifndef BBCLAW_STATUS_LED_GPIO_ON_LEVEL
#define BBCLAW_STATUS_LED_GPIO_ON_LEVEL 1
#endif

#ifndef BBCLAW_STATUS_LED_BRIGHTNESS_PCT
#define BBCLAW_STATUS_LED_BRIGHTNESS_PCT 3
#endif

/** 开机 RYG 跑马灯：依次点亮 R→Y→G，每色停留步长；整段时长 = 3 × 步长 × 圈数 */
#ifndef BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE
#define BBCLAW_STATUS_LED_BOOT_ANIM_ENABLE 1
#endif
#ifndef BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS
#define BBCLAW_STATUS_LED_BOOT_ANIM_STEP_MS 200
#endif
#ifndef BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS
#define BBCLAW_STATUS_LED_BOOT_ANIM_LOOPS 2
#endif

/**
 * WS2812 single-wire mode detection:
 * When RGB_MODULE=0 (not RGB module) AND Y_GPIO<0 AND G_GPIO<0,
 * it means single-wire WS2812 mode (using R_GPIO as the single data pin).
 * For BBClaw board: GPIO5 is WS2812 single-wire DIN.
 */
#ifndef BBCLAW_STATUS_LED_WS2812
#if !BBCLAW_STATUS_LED_KIND_RGB_MODULE && BBCLAW_STATUS_LED_Y_GPIO < 0 && BBCLAW_STATUS_LED_G_GPIO < 0
#define BBCLAW_STATUS_LED_WS2812 1
#else
#define BBCLAW_STATUS_LED_WS2812 0
#endif
#endif

#ifndef BBCLAW_AUDIO_SAMPLE_RATE
#define BBCLAW_AUDIO_SAMPLE_RATE 16000
#endif

#ifndef BBCLAW_AUDIO_I2S_DMA_DESC_NUM
#define BBCLAW_AUDIO_I2S_DMA_DESC_NUM 4
#endif

#ifndef BBCLAW_AUDIO_I2S_DMA_FRAME_NUM
#define BBCLAW_AUDIO_I2S_DMA_FRAME_NUM 120
#endif

#ifndef BBCLAW_AUDIO_INPUT_SOURCE
#define BBCLAW_AUDIO_INPUT_SOURCE "inmp441"
#endif

#ifndef BBCLAW_AUDIO_CHANNELS
#define BBCLAW_AUDIO_CHANNELS 1
#endif

#ifndef BBCLAW_AUDIO_RX_STEREO_CAPTURE
#define BBCLAW_AUDIO_RX_STEREO_CAPTURE 1
#endif

#ifndef BBCLAW_AUDIO_FRAME_MS
#define BBCLAW_AUDIO_FRAME_MS 20
#endif

#ifndef BBCLAW_STREAM_CHUNK_MS
#define BBCLAW_STREAM_CHUNK_MS 60
#endif

#ifndef BBCLAW_AUDIO_I2S_SLOT_BITS
#define BBCLAW_AUDIO_I2S_SLOT_BITS 16
#endif

#ifndef BBCLAW_AUDIO_RX_SHIFT_BITS
#define BBCLAW_AUDIO_RX_SHIFT_BITS 16
#endif

#ifndef BBCLAW_AUDIO_RX_MONO_PICK_RIGHT
#define BBCLAW_AUDIO_RX_MONO_PICK_RIGHT 0
#endif

#ifndef BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK
#define BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK 1
#endif

#ifndef BBCLAW_AUDIO_RX_WARMUP_SAMPLES
#define BBCLAW_AUDIO_RX_WARMUP_SAMPLES 256
#endif

#ifndef BBCLAW_AUDIO_INMP441_GAIN_NUM
#define BBCLAW_AUDIO_INMP441_GAIN_NUM 8
#endif

#ifndef BBCLAW_AUDIO_INMP441_GAIN_DEN
#define BBCLAW_AUDIO_INMP441_GAIN_DEN 1
#endif

#ifndef BBCLAW_AUDIO_INMP441_HPF_ENABLE
#define BBCLAW_AUDIO_INMP441_HPF_ENABLE 1
#endif

#ifndef BBCLAW_AUDIO_INMP441_HPF_ALPHA_Q15
#define BBCLAW_AUDIO_INMP441_HPF_ALPHA_Q15 32113
#endif

#ifndef BBCLAW_AUDIO_DIAG_LOG_INTERVAL_MS
#define BBCLAW_AUDIO_DIAG_LOG_INTERVAL_MS 1000
#endif

#ifndef BBCLAW_AUDIO_TX_EXPERIMENT
/* 1=A stereo32 [v,v], 2=B stereo16 [v,v], 3=C mono16 left-slot */
#define BBCLAW_AUDIO_TX_EXPERIMENT 1
#endif

#ifndef BBCLAW_AUDIO_IO_TIMEOUT_MS
#define BBCLAW_AUDIO_IO_TIMEOUT_MS 200
#endif

#ifndef BBCLAW_AUDIO_TX_RATE_COMP_NUM
#define BBCLAW_AUDIO_TX_RATE_COMP_NUM 1
#endif

#ifndef BBCLAW_AUDIO_TX_RATE_COMP_DEN
#define BBCLAW_AUDIO_TX_RATE_COMP_DEN 1
#endif

#ifndef BBCLAW_VAD_ENABLE
#define BBCLAW_VAD_ENABLE 1
#endif

#ifndef BBCLAW_VAD_MIN_DURATION_MS
#define BBCLAW_VAD_MIN_DURATION_MS 500
#endif

#ifndef BBCLAW_VAD_MIN_NONZERO_PERMILLE
#define BBCLAW_VAD_MIN_NONZERO_PERMILLE 40
#endif

#ifndef BBCLAW_VAD_MIN_MEAN_ABS
#define BBCLAW_VAD_MIN_MEAN_ABS 120
#endif

/** PTT 按下后先本地开麦；仅当累计特征满足下列门限才调用 adapter stream/start（避免无声占满并发流） */
#ifndef BBCLAW_VAD_ARM_MIN_DURATION_MS
#define BBCLAW_VAD_ARM_MIN_DURATION_MS 240
#endif

#ifndef BBCLAW_VAD_ARM_MIN_NONZERO_PERMILLE
#define BBCLAW_VAD_ARM_MIN_NONZERO_PERMILLE 40
#endif

#ifndef BBCLAW_VAD_ARM_MIN_MEAN_ABS
#define BBCLAW_VAD_ARM_MIN_MEAN_ABS 80
#endif

/** 预检阶段最长等待（毫秒），超时则放弃本轮（需松键再按）；0 表示不限制 */
#ifndef BBCLAW_VAD_ARM_MAX_WAIT_MS
#define BBCLAW_VAD_ARM_MAX_WAIT_MS 15000
#endif

/** 锁屏密语验证：采集的最长 PCM 时长（毫秒），超出部分丢弃并标记 truncated */
#ifndef BBCLAW_VOICE_VERIFY_MAX_MS
#define BBCLAW_VOICE_VERIFY_MAX_MS 4000
#endif

#ifndef BBCLAW_HTTP_TIMEOUT_MS
#define BBCLAW_HTTP_TIMEOUT_MS 5000
#endif

#ifndef BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS
#define BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS 90000
#endif

#ifndef BBCLAW_ADAPTER_HEARTBEAT_INTERVAL_MS
#define BBCLAW_ADAPTER_HEARTBEAT_INTERVAL_MS 5000
#endif

#ifndef BBCLAW_ADAPTER_HEARTBEAT_FAIL_THRESHOLD
#define BBCLAW_ADAPTER_HEARTBEAT_FAIL_THRESHOLD 2
#endif

#ifndef BBCLAW_HTTP_RETRY_COUNT
#define BBCLAW_HTTP_RETRY_COUNT 2
#endif

#ifndef BBCLAW_HTTP_RETRY_DELAY_MS
#define BBCLAW_HTTP_RETRY_DELAY_MS 200
#endif

#ifndef BBCLAW_ADAPTER_BOOT_HEALTH_RETRIES
#define BBCLAW_ADAPTER_BOOT_HEALTH_RETRIES 8
#endif

#ifndef BBCLAW_ADAPTER_BOOT_HEALTH_DELAY_MS
#define BBCLAW_ADAPTER_BOOT_HEALTH_DELAY_MS 500
#endif

#ifndef BBCLAW_ES8311_I2C_PORT
#define BBCLAW_ES8311_I2C_PORT 0
#endif

#ifndef BBCLAW_ES8311_I2C_SDA_GPIO
#define BBCLAW_ES8311_I2C_SDA_GPIO 8
#endif

#ifndef BBCLAW_ES8311_I2C_SCL_GPIO
#define BBCLAW_ES8311_I2C_SCL_GPIO 6
#endif

#ifndef BBCLAW_ES8311_I2C_ADDR
#define BBCLAW_ES8311_I2C_ADDR 0x18
#endif

#ifndef BBCLAW_AUDIO_I2S_MCK_GPIO
#ifdef BBCLAW_ES8311_I2S_MCK_GPIO
#define BBCLAW_AUDIO_I2S_MCK_GPIO BBCLAW_ES8311_I2S_MCK_GPIO
#else
#define BBCLAW_AUDIO_I2S_MCK_GPIO 2
#endif
#endif

#ifndef BBCLAW_AUDIO_I2S_BCK_GPIO
#ifdef BBCLAW_ES8311_I2S_BCK_GPIO
#define BBCLAW_AUDIO_I2S_BCK_GPIO BBCLAW_ES8311_I2S_BCK_GPIO
#else
#define BBCLAW_AUDIO_I2S_BCK_GPIO 16
#endif
#endif

#ifndef BBCLAW_AUDIO_I2S_WS_GPIO
#ifdef BBCLAW_ES8311_I2S_WS_GPIO
#define BBCLAW_AUDIO_I2S_WS_GPIO BBCLAW_ES8311_I2S_WS_GPIO
#else
#define BBCLAW_AUDIO_I2S_WS_GPIO 15
#endif
#endif

#ifndef BBCLAW_AUDIO_I2S_DO_GPIO
#ifdef BBCLAW_ES8311_I2S_DO_GPIO
#define BBCLAW_AUDIO_I2S_DO_GPIO BBCLAW_ES8311_I2S_DO_GPIO
#else
#define BBCLAW_AUDIO_I2S_DO_GPIO 17
#endif
#endif

#ifndef BBCLAW_AUDIO_I2S_DI_GPIO
#ifdef BBCLAW_ES8311_I2S_DI_GPIO
#define BBCLAW_AUDIO_I2S_DI_GPIO BBCLAW_ES8311_I2S_DI_GPIO
#else
#define BBCLAW_AUDIO_I2S_DI_GPIO 18
#endif
#endif

#ifndef BBCLAW_ES8311_I2S_MCK_GPIO
#define BBCLAW_ES8311_I2S_MCK_GPIO BBCLAW_AUDIO_I2S_MCK_GPIO
#endif

#ifndef BBCLAW_ES8311_I2S_BCK_GPIO
#define BBCLAW_ES8311_I2S_BCK_GPIO BBCLAW_AUDIO_I2S_BCK_GPIO
#endif

#ifndef BBCLAW_ES8311_I2S_WS_GPIO
#define BBCLAW_ES8311_I2S_WS_GPIO BBCLAW_AUDIO_I2S_WS_GPIO
#endif

#ifndef BBCLAW_ES8311_I2S_DO_GPIO
#define BBCLAW_ES8311_I2S_DO_GPIO BBCLAW_AUDIO_I2S_DO_GPIO
#endif

#ifndef BBCLAW_ES8311_I2S_DI_GPIO
#define BBCLAW_ES8311_I2S_DI_GPIO BBCLAW_AUDIO_I2S_DI_GPIO
#endif

/* ------------------------------------------------------------------
 * Mic / Speaker silk-label aliases (INMP441 + MAX98357A)
 *
 * These aliases map 1:1 to BBCLAW_AUDIO_I2S_* so firmware has a single
 * source of truth, while boards / docs can refer to the pin names that
 * are actually silk-printed on each module:
 *
 *   INMP441 mic silk  → ESP I2S role       → macro
 *     SCK             → I2S BCLK           → BBCLAW_MIC_SCK_GPIO
 *     WS              → I2S WS/LRCK        → BBCLAW_MIC_WS_GPIO
 *     SD              → I2S data in (RX)   → BBCLAW_MIC_SD_GPIO
 *     VDD             → 3V3                → (power, not a GPIO)
 *     GND             → GND                → (power, not a GPIO)
 *     L/R             → GND or 3V3         → (strap, not a GPIO)
 *
 *   MAX98357A speaker silk → ESP I2S role   → macro
 *     BCLK            → I2S BCLK           → BBCLAW_SPK_BCLK_GPIO
 *     LRC             → I2S WS/LRCK        → BBCLAW_SPK_LRC_GPIO
 *     DIN             → I2S data out (TX)  → BBCLAW_SPK_DIN_GPIO
 *     SD              → shutdown / enable  → BBCLAW_SPK_SD_GPIO  (alias of BBCLAW_SPEAKER_SW_GPIO)
 *     VIN             → 5V (recommended)   → (power, not a GPIO)
 *     GND             → GND                → (power, not a GPIO)
 *     GAIN            → strap (typ. float) → (not a GPIO)
 *
 * BCLK and WS/LRC are the shared I2S clock lines between mic and speaker,
 * so BBCLAW_MIC_SCK_GPIO == BBCLAW_SPK_BCLK_GPIO and
 *    BBCLAW_MIC_WS_GPIO  == BBCLAW_SPK_LRC_GPIO by construction.
 * ------------------------------------------------------------------ */

#ifndef BBCLAW_MIC_SCK_GPIO
#define BBCLAW_MIC_SCK_GPIO BBCLAW_AUDIO_I2S_BCK_GPIO
#endif

#ifndef BBCLAW_MIC_WS_GPIO
#define BBCLAW_MIC_WS_GPIO BBCLAW_AUDIO_I2S_WS_GPIO
#endif

#ifndef BBCLAW_MIC_SD_GPIO
#define BBCLAW_MIC_SD_GPIO BBCLAW_AUDIO_I2S_DI_GPIO
#endif

#ifndef BBCLAW_SPK_BCLK_GPIO
#define BBCLAW_SPK_BCLK_GPIO BBCLAW_AUDIO_I2S_BCK_GPIO
#endif

#ifndef BBCLAW_SPK_LRC_GPIO
#define BBCLAW_SPK_LRC_GPIO BBCLAW_AUDIO_I2S_WS_GPIO
#endif

#ifndef BBCLAW_SPK_DIN_GPIO
#define BBCLAW_SPK_DIN_GPIO BBCLAW_AUDIO_I2S_DO_GPIO
#endif

#ifndef BBCLAW_SPK_SD_GPIO
#define BBCLAW_SPK_SD_GPIO BBCLAW_SPEAKER_SW_GPIO
#endif

#ifndef BBCLAW_ST7789_HOST
#define BBCLAW_ST7789_HOST 2
#endif

#ifndef BBCLAW_ST7789_SCLK_GPIO
#define BBCLAW_ST7789_SCLK_GPIO 12
#endif

#ifndef BBCLAW_ST7789_MOSI_GPIO
#define BBCLAW_ST7789_MOSI_GPIO 11
#endif

#ifndef BBCLAW_ST7789_CS_GPIO
#define BBCLAW_ST7789_CS_GPIO 10
#endif

#ifndef BBCLAW_ST7789_DC_GPIO
#define BBCLAW_ST7789_DC_GPIO 9
#endif

#ifndef BBCLAW_ST7789_RST_GPIO
#define BBCLAW_ST7789_RST_GPIO 14
#endif

#ifndef BBCLAW_ST7789_BL_GPIO
#define BBCLAW_ST7789_BL_GPIO 13
#endif

#ifndef BBCLAW_ST7789_WIDTH
#define BBCLAW_ST7789_WIDTH 320
#endif

#ifndef BBCLAW_ST7789_HEIGHT
#define BBCLAW_ST7789_HEIGHT 172
#endif

/** ST7789 / LVGL「ME / AI」各一栏的 UTF-8 缓冲（助手常带多行 Markdown，200 易截断） */
#ifndef BBCLAW_DISPLAY_CHAT_LINE_LEN
#define BBCLAW_DISPLAY_CHAT_LINE_LEN 512
#endif

/** 保留最近若干轮对话，供左右切换回看（仅固件 RAM，与 LVGL 历史无关） */
#ifndef BBCLAW_DISPLAY_CHAT_HISTORY
#define BBCLAW_DISPLAY_CHAT_HISTORY 8
#endif

/** READY 状态下无操作多久后自动回到待机画面（清掉聊天历史），单位 ms；0 = 不自动回待机 */
#ifndef BBCLAW_DISPLAY_STANDBY_TIMEOUT_MS
#define BBCLAW_DISPLAY_STANDBY_TIMEOUT_MS 120000
#endif

/**
 * 屏幕旋转角（度）：0 / 90 / 180 / 270。由板级 board_config.h 映射到 ST7789 swap/mirror。
 * 未在板级定义时默认为 0（与仅写死 SWAP/MIRROR 的旧板卡行为一致）。
 */
#ifndef BBCLAW_DISPLAY_ROTATION_DEG
#define BBCLAW_DISPLAY_ROTATION_DEG 0
#endif

/**
 * Display orientation defaults — boards define these in board_config.h.
 * These are fallbacks only.
 */
#ifndef BBCLAW_ST7789_X_GAP
#define BBCLAW_ST7789_X_GAP 0
#endif

#ifndef BBCLAW_ST7789_Y_GAP
#define BBCLAW_ST7789_Y_GAP 34
#endif

#ifndef BBCLAW_ST7789_PCLK_HZ
#define BBCLAW_ST7789_PCLK_HZ (20 * 1000 * 1000)
#endif

#ifndef BBCLAW_ST7789_SWAP_XY
#define BBCLAW_ST7789_SWAP_XY 1
#endif

#ifndef BBCLAW_ST7789_MIRROR_X
#define BBCLAW_ST7789_MIRROR_X 0
#endif

#ifndef BBCLAW_ST7789_MIRROR_Y
#define BBCLAW_ST7789_MIRROR_Y 0
#endif

#ifndef BBCLAW_ST7789_INVERT_COLOR
#define BBCLAW_ST7789_INVERT_COLOR 0
#endif

#ifndef BBCLAW_ST7789_RGB_ORDER_BGR
#define BBCLAW_ST7789_RGB_ORDER_BGR 0
#endif

#ifndef BBCLAW_ST7789_SWAP_BYTES
#define BBCLAW_ST7789_SWAP_BYTES 1
#endif

/**
 * PSRAM 内存分配宏：
 * - 当 CONFIG_SPIRAM=y 时，使用 MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT（优先 PSRAM）
 * - 当 CONFIG_SPIRAM=n 时，仅使用 MALLOC_CAP_8BIT（回退到内部 RAM）
 */
#ifndef BBCLAW_MALLOC_CAP_PREFER_PSRAM
#ifdef CONFIG_SPIRAM
#define BBCLAW_MALLOC_CAP_PREFER_PSRAM (MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT)
#else
#define BBCLAW_MALLOC_CAP_PREFER_PSRAM (MALLOC_CAP_8BIT)
#endif
#endif
