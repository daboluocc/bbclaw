#pragma once

#include "sdkconfig.h"

/* ── Board selection: include board-specific pin map and hardware config ── */
#if defined(CONFIG_BBCLAW_BOARD_ATK_DNESP32S3_BOX)
#include "../boards/atk-dnesp32s3-box/board_config.h"
#elif defined(CONFIG_BBCLAW_BOARD_WS_AMOLED_206)
#include "../boards/waveshare-amoled-206/board_config.h"
#elif defined(CONFIG_BBCLAW_BOARD_LICHUANG_SZP)
#include "../boards/lichuang-szp/board_config.h"
#elif defined(CONFIG_BBCLAW_BOARD_M5STICKS3)
#include "../boards/m5sticks3/board_config.h"
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
#ifndef BBCLAW_DISPLAY_BUS_QSPI
#define BBCLAW_DISPLAY_BUS_QSPI 0
#endif

/* ── AXP2101 PMIC minimal init (MIC rail ALDO1 + charger config), see bb_audio.c ── */
#ifndef BBCLAW_AXP2101_MINIMAL_INIT
#define BBCLAW_AXP2101_MINIMAL_INIT 0
#endif

/* ── M5PM1 PMIC minimal init (M5StickS3): 开 L3B 轨(GPIO2=屏/mic/spk 电源)+ 配功放
 *    GPIO3 + 注册 PA 钩子，跑在探 ES8311 之前。见 bb_audio.c。 ── */
#ifndef BBCLAW_M5PM1_MINIMAL_INIT
#define BBCLAW_M5PM1_MINIMAL_INIT 0
#endif

/* ── PCA9557 IO expander（实战派：LCD_CS + PA_EN 挂它上面），见 bb_pca9557.c ── */
#ifndef BBCLAW_PCA9557_ENABLE
#define BBCLAW_PCA9557_ENABLE 0
#endif

/* ── ST7789 背光有效电平（实战派为反相驱动=0；旧板全部高有效） ── */
#ifndef BBCLAW_ST7789_BL_ACTIVE_LEVEL
#define BBCLAW_ST7789_BL_ACTIVE_LEVEL 1
#endif

/* ── ST7789 SPI mode（CPOL/CPHA）：实战派面板要 mode 2，其余板 mode 0 ── */
#ifndef BBCLAW_ST7789_SPI_MODE
#define BBCLAW_ST7789_SPI_MODE 0
#endif

/* ── 触摸坐标变换（横屏板需与显示 MADCTL 同步;竖屏板全 0）── */
#ifndef BBCLAW_TOUCH_SWAP_XY
#define BBCLAW_TOUCH_SWAP_XY 0
#endif
#ifndef BBCLAW_TOUCH_MIRROR_X
#define BBCLAW_TOUCH_MIRROR_X 0
#endif
#ifndef BBCLAW_TOUCH_MIRROR_Y
#define BBCLAW_TOUCH_MIRROR_Y 0
#endif

/* ── ST7789 背光 PWM 驱动（实战派:升压电路要开关信号,恒定电平不亮）── */
#ifndef BBCLAW_ST7789_BL_PWM
#define BBCLAW_ST7789_BL_PWM 0
#endif
#ifndef BBCLAW_ST7789_BL_PWM_FREQ_HZ
#define BBCLAW_ST7789_BL_PWM_FREQ_HZ 5000
#endif
#ifndef BBCLAW_ST7789_BL_PWM_ON_DUTY
#define BBCLAW_ST7789_BL_PWM_ON_DUTY 512 /* 10-bit,50%(实战派实测点亮值) */
#endif

/* ── IMU（QMI8658）+ 息屏管理默认值:无此硬件的板自动降级(模块内自门控) ── */
#ifndef BBCLAW_IMU_ENABLE
#define BBCLAW_IMU_ENABLE 0
#endif
#ifndef BBCLAW_IMU_QMI8658_I2C_ADDR
#define BBCLAW_IMU_QMI8658_I2C_ADDR 0x6B
#endif
#ifndef BBCLAW_IMU_SAMPLE_RATE_HZ
#define BBCLAW_IMU_SAMPLE_RATE_HZ 100
#endif
#ifndef BBCLAW_DISPLAY_BRIGHTNESS_CONTROL
#define BBCLAW_DISPLAY_BRIGHTNESS_CONTROL 0
#endif

/* Keep the low-brightness ambient standby view visible after the idle timeout.
 * Boards that leave this off retain the conventional DIMMING -> SLEEPING flow. */
#ifndef BBCLAW_SLEEP_MANAGER_AMBIENT_STANDBY
#define BBCLAW_SLEEP_MANAGER_AMBIENT_STANDBY 0
#endif

/* ── CPU/系统级低功耗（ADR-047）:自动 light sleep + DFS。默认关,仅带电池且验证过
 * 的板打开(需同时在 sdkconfig 开 CONFIG_PM_ENABLE)。bb_pm 模块内自门控为 no-op。 ── */
#ifndef BBCLAW_PM_LIGHT_SLEEP_ENABLE
#define BBCLAW_PM_LIGHT_SLEEP_ENABLE 0
#endif
#ifndef BBCLAW_PM_MAX_FREQ_MHZ
#define BBCLAW_PM_MAX_FREQ_MHZ 240
#endif
#ifndef BBCLAW_PM_MIN_FREQ_MHZ
#define BBCLAW_PM_MIN_FREQ_MHZ 80
#endif

/* ── PWR 键（AXP2101 PKEY 路由到 GPIO,短按脉冲）：录音一键启停,see bb_radio_app ── */
#ifndef BBCLAW_PWR_KEY_GPIO
#define BBCLAW_PWR_KEY_GPIO (-1)
#endif
#ifndef BBCLAW_PWR_KEY_ACTIVE_LEVEL
#define BBCLAW_PWR_KEY_ACTIVE_LEVEL 1
#endif

/* ── Micro SD（SDMMC 1-bit）：录音本地优先存储，see bb_sdcard.c / ADR-044 ── */
#ifndef BBCLAW_SDMMC_ENABLE
#define BBCLAW_SDMMC_ENABLE 0
#endif
#ifndef BBCLAW_SDMMC_CLK_GPIO
#define BBCLAW_SDMMC_CLK_GPIO (-1)
#endif
#ifndef BBCLAW_SDMMC_CMD_GPIO
#define BBCLAW_SDMMC_CMD_GPIO (-1)
#endif
#ifndef BBCLAW_SDMMC_D0_GPIO
#define BBCLAW_SDMMC_D0_GPIO (-1)
#endif

/* ── 电量数据源：AXP2101 硬件电量计（替代 ADC 分压），see bb_power.c ── */
#ifndef BBCLAW_POWER_SOURCE_AXP2101
#define BBCLAW_POWER_SOURCE_AXP2101 0
#endif

/* 「本板支持电池显示」统一判定（UI 与数据层共用） */
#define BBCLAW_POWER_SUPPORTED \
  (BBCLAW_POWER_ENABLE && ((BBCLAW_POWER_ADC_GPIO >= 0) || BBCLAW_POWER_SOURCE_AXP2101))

/* ── ES7210 四通道 ADC（mic 不走 ES8311 的板子，如手表），see bb_audio.c ── */
#ifndef BBCLAW_ES7210_ENABLE
#define BBCLAW_ES7210_ENABLE 0
#endif

/* ── 圆角标定 overlay（一次性工具）：开=四角画编号弧，肉眼定 CORNER_RADIUS ── */
#ifndef BBCLAW_UI_CORNER_CAL
#define BBCLAW_UI_CORNER_CAL 0
#endif

/* ── FT5x06 兼容触控（手表 FT3168）→ 手势注入导航事件，see bb_touch_input.c ── */
#ifndef BBCLAW_TOUCH_FT5X06_ENABLE
#define BBCLAW_TOUCH_FT5X06_ENABLE 0
#endif
#ifndef BBCLAW_TOUCH_RST_GPIO
#define BBCLAW_TOUCH_RST_GPIO (-1)
#endif
#ifndef BBCLAW_TOUCH_INT_GPIO
#define BBCLAW_TOUCH_INT_GPIO (-1)
#endif

/* ── OTA platform tag reported to /v1/ota/check ──
 * 云端按 platform 匹配 active release；拓展板必须报独立平台名，否则会被推
 * bbclaw 正式板固件（引脚不同 → 黑屏）。默认值保持既有行为。 */
#ifndef BBCLAW_OTA_PLATFORM
#define BBCLAW_OTA_PLATFORM "esp32s3"
#endif

/* ── Display refresh-area pixel alignment (1 = none; CO5300 AMOLED needs 2) ── */
#ifndef BBCLAW_DISPLAY_PIXEL_ALIGN
#define BBCLAW_DISPLAY_PIXEL_ALIGN 1
#endif

/* ── LVGL draw buffer shape (lines per buffer / placement / double buffering) ── */
#ifndef BBCLAW_LVGL_BUFF_LINES
#define BBCLAW_LVGL_BUFF_LINES 40
#endif
#ifndef BBCLAW_LVGL_BUFF_SPIRAM
#define BBCLAW_LVGL_BUFF_SPIRAM 0
#endif
#ifndef BBCLAW_LVGL_BUFF_DOUBLE
#define BBCLAW_LVGL_BUFF_DOUBLE 1
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

/* 每个 SSID 失败回退前的重试次数（issue #149）。过期的已存网络（如换过的旧
 * SSID）每次开机会白白重试满这个次数（每次 ~2.5s）才回退到下一个 —— 3 次≈10s
 * 拖慢启动。降到 2：对真实网络仍留一次重试容错，又能更快越过失效的已存网络。 */
#ifndef BBCLAW_WIFI_STA_MAX_RETRY
#define BBCLAW_WIFI_STA_MAX_RETRY 2
#endif

/* ── 运行期自动重连（issue #170）──────────────────────────────────────────
 * 设备在 STA 已连接状态下掉线后，先做 BBCLAW_WIFI_STA_MAX_RETRY 次快速重试
 * （在事件回调里直接调 esp_wifi_connect()），耗尽后进入指数退避 backoff timer：
 *   5s → 10s → 30s → 60s → 300s（封顶），持续重试直到 IP 取到为止。
 * 只在运行期掉线时激活；首次启动失败后走原有逻辑（遍历 SSID → 进 AP 配网）。 */

/* 退避起始间隔（毫秒） */
#ifndef BBCLAW_WIFI_RECONNECT_BACKOFF_MIN_MS
#ifdef CONFIG_BBCLAW_WIFI_RECONNECT_BACKOFF_MIN_MS
#define BBCLAW_WIFI_RECONNECT_BACKOFF_MIN_MS CONFIG_BBCLAW_WIFI_RECONNECT_BACKOFF_MIN_MS
#else
#define BBCLAW_WIFI_RECONNECT_BACKOFF_MIN_MS 5000U
#endif
#endif

/* 退避上限（毫秒） */
#ifndef BBCLAW_WIFI_RECONNECT_BACKOFF_MAX_MS
#ifdef CONFIG_BBCLAW_WIFI_RECONNECT_BACKOFF_MAX_MS
#define BBCLAW_WIFI_RECONNECT_BACKOFF_MAX_MS CONFIG_BBCLAW_WIFI_RECONNECT_BACKOFF_MAX_MS
#else
#define BBCLAW_WIFI_RECONNECT_BACKOFF_MAX_MS 300000U
#endif
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

/* Per-slot last-connected sequence counter key prefix.
 * Keys are generated as "sta_ts_0" .. "sta_ts_3" via nvs_slot_key().
 * Stored as uint64_t; higher value = more recently connected.
 * Value 0 means "never connected via this slot". */
#ifndef BBCLAW_WIFI_NVS_KEY_LAST_TS
#define BBCLAW_WIFI_NVS_KEY_LAST_TS "sta_ts"
#endif

/* Global monotonic connection counter key (uint64_t). */
#ifndef BBCLAW_WIFI_NVS_KEY_CONN_SEQ
#define BBCLAW_WIFI_NVS_KEY_CONN_SEQ "conn_seq"
#endif

#ifndef BBCLAW_WIFI_MAX_SAVED
/* Field test 2026-07-04: 4 was too few — a device that roams home/office/hotspot
 * hit the ceiling and couldn't add the phone hotspot on site. NVS slot keys go up
 * to "sta_pass_15" (11 chars, fits key[16]); 8 is plenty and cheap. */
#define BBCLAW_WIFI_MAX_SAVED 8
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

/* softAP 配网启动前的内部 DMA 堆门槛(#252 boot-loop)。softAP 的 beacon/mgmt
 * 缓冲必须从内部 DMA RAM 拿(PSRAM 不能做 wifi DMA)。内部堆碎片化到分不出时,
 * esp_wifi_start() 会在闭源 wifi 库 ieee80211_hostap_attach 对 NULL strlen 崩溃
 * (LoadProhibited)→ 无 wifi 时无限 boot loop。低于门槛则不启动 AP、优雅返错
 * (bb_radio_app 捕获→停 WiFi 错误页,不 abort)。门槛凭真机 softap pre-start
 * 日志调:宁可保守拒启(错误页)也不让 wifi 库崩。 */
#ifndef BBCLAW_WIFI_AP_MIN_DMA_LARGEST
#define BBCLAW_WIFI_AP_MIN_DMA_LARGEST 4096
#endif

#ifndef BBCLAW_WIFI_AP_MIN_DMA_FREE
#define BBCLAW_WIFI_AP_MIN_DMA_FREE 12288
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
#elif defined(CONFIG_BBCLAW_TRANSPORT_PROFILE_LOCAL_HOME_V2)
#define BBCLAW_TRANSPORT_PROFILE "local_home_v2"
#else
#define BBCLAW_TRANSPORT_PROFILE "local_home"
#endif
#endif

#ifndef BBCLAW_ADAPTER_V2_BASE_URL
#ifdef CONFIG_BBCLAW_ADAPTER_V2_BASE_URL
#define BBCLAW_ADAPTER_V2_BASE_URL CONFIG_BBCLAW_ADAPTER_V2_BASE_URL
#else
#define BBCLAW_ADAPTER_V2_BASE_URL "ws://192.168.16.204:18090"
#endif
#endif

#ifndef BBCLAW_CLOUD_BASE_URL
#ifdef CONFIG_BBCLAW_CLOUD_BASE_URL
#define BBCLAW_CLOUD_BASE_URL CONFIG_BBCLAW_CLOUD_BASE_URL
#else
#define BBCLAW_CLOUD_BASE_URL "http://bbclaw.daboluo.cc"
#endif
#endif

#ifndef BBCLAW_CLOUD_AUDIO_STREAMING_READY
#define BBCLAW_CLOUD_AUDIO_STREAMING_READY 1
#endif

/** 为 1 时：每包 /v1/stream/chunk 打印 wall_span_ms / gap_prev_ms / http_ms，便于对照服务端 AUDIO_TOO_LONG */
#ifndef BBCLAW_ADAPTER_STREAM_CHUNK_DIAG
#define BBCLAW_ADAPTER_STREAM_CHUNK_DIAG 1
#endif

/** 为 1 时：打印 TTS 逐包调试日志（phase=tts_chunk_* / play_pcm stereo32）。
 *  默认 0：只保留一次性里程碑与错误日志，避免 30~60 条/回答 的刷屏。 */
#ifndef BBCLAW_DEBUG_TTS_LOG
#define BBCLAW_DEBUG_TTS_LOG 0
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

/** 开机点阵动画（诺基亚式 "BBCLAW" 逐列扫亮，见 STATE_MACHINE.md §3.5）。0=关闭 */
#ifndef BBCLAW_BOOT_SPLASH_ENABLE
#define BBCLAW_BOOT_SPLASH_ENABLE 1
#endif

/** 开机语音相对动画开始的最小延迟（ms）——压在扫列完成之后一点 */
#ifndef BBCLAW_BOOT_SPLASH_VOICE_DELAY_MS
#define BBCLAW_BOOT_SPLASH_VOICE_DELAY_MS 1150
#endif

/** 开机动画最短展示时长（ms），语音播完后不足则补足再硬切 */
#ifndef BBCLAW_BOOT_SPLASH_MIN_MS
#define BBCLAW_BOOT_SPLASH_MIN_MS 2600
#endif

/** MIN_MS 之后再等动画真正收尾的上限（ms）——LVGL task 被音频饿到时
 *  扫列节拍会落后墙钟，靠 bb_page_boot_anim_done() 轮询补齐 */
#ifndef BBCLAW_BOOT_SPLASH_ANIM_GRACE_MS
#define BBCLAW_BOOT_SPLASH_ANIM_GRACE_MS 2000
#endif

/** 网络连接点阵动画页（WiFi 弧 + 当前 SSID，见 STATE_MACHINE.md §3.5.1）。0=关闭 */
#ifndef BBCLAW_NETCONN_PAGE_ENABLE
#define BBCLAW_NETCONN_PAGE_ENABLE 1
#endif

/** WiFi 连上后等待 SNTP 时间就绪的上限（ms），超时自销毁露出待机页 */
#ifndef BBCLAW_NETCONN_SYNC_TIMEOUT_MS
#define BBCLAW_NETCONN_SYNC_TIMEOUT_MS 10000
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

/* PTT debounce window (ADR-028: target ~10ms capture). Decoupled from the
 * poll granularity below so short taps aren't dropped and release latency stays
 * low: a level change is confirmed after BBCLAW_PTT_DEBOUNCE_MS /
 * BBCLAW_PTT_POLL_MS consecutive stable samples. Previously this value WAS the
 * poll period (30ms) and the code required 2 samples ≈ 60ms, which silently
 * dropped <60ms taps ("按了没反应") and lagged release ~60ms ("松手仍在录"). */
#ifndef BBCLAW_PTT_DEBOUNCE_MS
#define BBCLAW_PTT_DEBOUNCE_MS 10
#endif

#ifndef BBCLAW_PTT_POLL_MS
#define BBCLAW_PTT_POLL_MS 5
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

/* Auto-repeat for UP/DOWN scroll keys. Other directional keys (LEFT/RIGHT/OK/BACK)
 * do not repeat — re-firing those triggers driver cycling / re-opens picker /
 * cancels turns, all of which want a fresh tap each time.
 *   INITIAL: how long the user must hold UP/DOWN before auto-repeat kicks in.
 *   INTERVAL: gap between repeats once auto-repeat is active.
 * 650 / 100 ms gives ~10 scroll lines per second once held. INITIAL was 400ms,
 * short enough that a deliberate single tap held slightly long would already
 * auto-repeat ("长按意外连续滚动"); 650ms requires an intentional hold. */
#ifndef BBCLAW_NAV_REPEAT_INITIAL_MS
#define BBCLAW_NAV_REPEAT_INITIAL_MS 650
#endif

#ifndef BBCLAW_NAV_REPEAT_INTERVAL_MS
#define BBCLAW_NAV_REPEAT_INTERVAL_MS 100
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

/** 电池电压 EMA 滤波系数（百分比，新采样值权重）。
 *  越小越平滑、跟随越慢。25 = 新值占 0.25，旧值占 0.75。 */
#ifndef BBCLAW_POWER_EMA_ALPHA_PCT
#define BBCLAW_POWER_EMA_ALPHA_PCT 25
#endif

/** 电量百分比迟滞：滤波后百分比相对上次显示值变化 >= 此值才更新，
 *  消除 ±1% 抖动。 */
#ifndef BBCLAW_POWER_HYSTERESIS_PCT
#define BBCLAW_POWER_HYSTERESIS_PCT 2
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
/* 8x saturated near-field speech (pcm diag showed max pegged at INT16_MAX with
 * heavy positive clipping), which can degrade cloud ASR. 4x leaves ~2 bits of
 * headroom while staying loud enough for the INMP441's low raw level.
 * 注：bench 板若出现"静音也 mean_abs≈20000、削顶严重、ASR 恒空",那是 mic 硬件
 * (SD/DOUT 悬空读噪声) 而非增益问题——查接线，别靠调此值。修好 mic 后再按真实
 * raw 电平重新整定。 */
#define BBCLAW_AUDIO_INMP441_GAIN_NUM 4
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

/* Mic-fault guard (floating INMP441 SD/DOUT or bad L/R level): a railed / garbage
 * mic reads as near-full-scale on essentially every sample. Real acoustic speech
 * NEVER sustains this — even loud near-field clips only on peaks while the mean
 * stays well below full scale. When >= CLIP_PERMILLE of samples sit at/above
 * CLIP_LEVEL *and* mean_abs >= MEAN_ABS, treat the capture as a hardware fault:
 * skip the cloud ASR round-trip (it would just return empty) and surface
 * "麦克风异常" so the cause is obvious instead of looking like silence.
 * Signature that motivated this: clipped≈962‰, mean_abs≈31868 (full scale 32768),
 * both channels near-equal huge energy. See docs/debug/inmp441-lr-board-notes.md. */
#ifndef BBCLAW_MIC_FAULT_DETECT_ENABLE
#define BBCLAW_MIC_FAULT_DETECT_ENABLE 1
#endif

/* Sample magnitude considered "railed" (clamp output is ±32767/-32768). */
#ifndef BBCLAW_MIC_FAULT_CLIP_LEVEL
#define BBCLAW_MIC_FAULT_CLIP_LEVEL 32000
#endif

/* Fraction (per-mille) of railed samples required to suspect a fault. 600‰ = 60%;
 * real speech peaks clip far below this, the floating-line case is ~960‰. */
#ifndef BBCLAW_MIC_FAULT_CLIP_PERMILLE
#define BBCLAW_MIC_FAULT_CLIP_PERMILLE 600
#endif

/* AND-gate: mean_abs must also be this high (≈73% of full scale). Loud-but-real
 * speech at 4x gain sits around mean_abs 5000–10000, so this is a wide margin. */
#ifndef BBCLAW_MIC_FAULT_MEAN_ABS
#define BBCLAW_MIC_FAULT_MEAN_ABS 24000
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

/** Barge-in grace (ADR-028): for this long after a turn enters cloud-wait (PTT
 * released, ASR/agent processing, TTS NOT yet speaking), a PTT-down is treated
 * as the user impatiently re-pressing — NOT a barge-in — so it does NOT cancel
 * the just-submitted turn. Once TTS is actually speaking, barge-in is always
 * live (no grace). After the grace expires with still no reply, a press counts
 * as a real abort of a stuck turn. Kills the "release → 5s silence → re-press →
 * cancel my own correct turn" self-barge-in churn. */
#ifndef BBCLAW_PTT_BARGE_IN_GRACE_MS
#define BBCLAW_PTT_BARGE_IN_GRACE_MS 2500
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

/* 5 分钟：AI 思考/多步工具调用经常超过 90s，之前的 90000ms 会让设备在
 * adapter/cloud 仍在等回复(靠 heartbeat 保活)时先本地判超时挂断——回复真正
 * 生成好之后设备已经不听了，内容就丢了。改成 5 分钟，匹配 adapter_v2 侧
 * cloudrelay.ReplyWait 的新默认值，做真正的等待上限。*/
#ifndef BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS
#define BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS 300000
#endif

/* WS 等待路径（cloud_saas / bbwire2）的「空闲超时」：自本回合最后一个流式事件
 * （status/delta/thinking/tool_call/tts/prompt 帧）起，静默超过此值才判
 * VOICE_SESSION_TIMEOUT；每个事件到达即续期，多步长回合不再死于总时长。
 * 取 5min 与 adapter_v2 ReplyWait 空闲语义对齐——注意长工具调用（如 4 分钟的
 * Bash）期间 adapter 层零事件下发（tool_call 只在工具启动时发一次，adapter→cloud
 * 心跳不进设备），不能激进调小。WS 传输层 ping 不算活动。 */
#ifndef BBCLAW_STREAM_FINISH_IDLE_TIMEOUT_MS
#define BBCLAW_STREAM_FINISH_IDLE_TIMEOUT_MS 300000
#endif

/* 绝对时长兜底（0=不限）：防御「事件永续但回合永不收尾」的病态上游。 */
#ifndef BBCLAW_STREAM_FINISH_MAX_TOTAL_MS
#define BBCLAW_STREAM_FINISH_MAX_TOTAL_MS 1800000
#endif

#ifndef BBCLAW_ADAPTER_HEARTBEAT_INTERVAL_MS
#define BBCLAW_ADAPTER_HEARTBEAT_INTERVAL_MS 5000
#endif

/* ── Idle timeout: dual-level standby → lock ── */
#ifndef BBCLAW_CHAT_IDLE_TIMEOUT_MS
#define BBCLAW_CHAT_IDLE_TIMEOUT_MS 120000        /* Issue #145: 30s→120s，对话设备读回复场景下 30s 偏短 */
#endif

#ifndef BBCLAW_STANDBY_LOCK_TIMEOUT_MS
#define BBCLAW_STANDBY_LOCK_TIMEOUT_MS 120000
#endif

/* ADR-039: 密语离线死锁兜底。锁屏(LOCKED)期间若云端持续不可达
 * (s_transport_health_ok==0) 超过此时长，自动解锁回 CHAT——密语只能云端
 * voice.verify 校验，离线必然解不开，宁可放行也不把主人锁死。15s 宽限避开
 * WiFi 短暂重连(一般 5–10s)误触发。 */
#ifndef BBCLAW_LOCKED_OFFLINE_AUTO_UNLOCK_MS
#define BBCLAW_LOCKED_OFFLINE_AUTO_UNLOCK_MS 15000
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
 * ES8311 codec register tuning parameters
 *
 * These macros control the register values written during es8311_init_sequence().
 * All values are validated against the ES8311 datasheet (rev 1.4) for 16 kHz
 * operation with a 4.096 MHz MCLK (MCLK_MULTIPLE_256 × 16 kHz).
 *
 * Board-specific board_config.h files may override any of these before
 * bb_config.h applies the defaults, allowing per-board tuning without
 * touching bb_audio.c.
 *
 * Register map summary (ES8311 datasheet §6):
 *   0x00  RESET          — chip reset control
 *   0x01  CLK_MANAGER_1  — master/slave, MCLK source
 *   0x02  CLK_MANAGER_2  — MCLK pre-divider (M1/M2)
 *   0x03  CLK_MANAGER_3  — ADC OSR (over-sampling ratio)
 *   0x04  CLK_MANAGER_4  — DAC OSR
 *   0x05  CLK_MANAGER_5  — ADC/DAC dividers
 *   0x06  CLK_MANAGER_6  — BCLK divider
 *   0x07  CLK_MANAGER_7  — LRCK divider high byte
 *   0x08  CLK_MANAGER_8  — LRCK divider low byte (N-1; 0xFF → N=256 → 16 kHz)
 *   0x09  SDPIN          — DAC serial data format
 *   0x0A  SDPOUT         — ADC serial data format
 *   0x0D  SYSTEM_1       — analog power control
 *   0x0E  SYSTEM_2       — power sequencing
 *   0x10  SYSTEM_4       — reference / bias
 *   0x11  SYSTEM_5       — reference / bias
 *   0x12  SYSTEM_6       — DAC enable
 *   0x13  SYSTEM_7       — analog path
 *   0x14  SYSTEM_8       — MIC PGA / input path
 *   0x15  ADC_1          — ADC ramp rate
 *   0x16  ADC_2          — ADC PGA gain (0x00=0dB … 0x3F=+36dB in 0.5dB steps)
 *   0x17  ADC_3          — ADC digital volume (0xBF = 0 dB unity)
 *   0x1B  ADC_7          — ADC HPF coefficient low
 *   0x1C  ADC_8          — ADC HPF coefficient high
 *   0x31  DAC_1          — DAC mute control
 *   0x32  DAC_2          — DAC digital volume (0xBF = 0 dB unity)
 *   0x37  DAC_7          — DAC ramp rate
 *   0x44  GPIO_SEL       — GPIO / reference path enable
 *   0x45  TEST_MODE      — test mode (must be 0x00 in production)
 */

/*
 * BBCLAW_ES8311_ADC_PGA_GAIN — MIC PGA gain, register 0x16 bits [5:0].
 * Each LSB = 0.5 dB; 0x00 = 0 dB, 0x3F = +31.5 dB.
 * Default 0x24 = 18 dB — validated on bbclaw v1 hardware as a good
 * starting point for the onboard electret mic at 16 kHz / 30 cm distance.
 * Increase toward 0x30 (24 dB) for quieter environments; decrease toward
 * 0x18 (12 dB) if clipping is observed.
 */
#ifndef BBCLAW_ES8311_ADC_PGA_GAIN
#define BBCLAW_ES8311_ADC_PGA_GAIN 0x24
#endif

/*
 * BBCLAW_ES8311_ADC_VOLUME — ADC digital volume, register 0x17.
 * 0xBF = 0 dB (unity); 0x00 = −95.5 dB; 0xFF = +32 dB.
 * Keep at unity (0xBF) unless board-level measurements show a need to
 * trim the digital stage.
 */
#ifndef BBCLAW_ES8311_ADC_VOLUME
#define BBCLAW_ES8311_ADC_VOLUME 0xBF
#endif

/*
 * BBCLAW_ES8311_DAC_VOLUME — DAC digital volume, register 0x32.
 * 0xBF = 0 dB (unity). Same encoding as ADC volume.
 */
#ifndef BBCLAW_ES8311_DAC_VOLUME
#define BBCLAW_ES8311_DAC_VOLUME 0xBF
#endif

/*
 * BBCLAW_ES8311_ADC_OSR — ADC over-sampling ratio, register 0x03.
 * 0x10 = OSR 32 (recommended for 16 kHz, datasheet Table 5).
 * Higher OSR improves SNR at the cost of power; 32 is the datasheet
 * default for voice-band operation.
 */
#ifndef BBCLAW_ES8311_ADC_OSR
#define BBCLAW_ES8311_ADC_OSR 0x10
#endif

/*
 * BBCLAW_ES8311_DAC_OSR — DAC over-sampling ratio, register 0x04.
 * 0x10 per espressif/es8311 coeff table row {4096000, 16000}（MCLK=256×fs）。
 * 旧默认 0x20 是 8 kHz 行的值，手表真机上 DAC 无声的元凶候选。
 */
#ifndef BBCLAW_ES8311_DAC_OSR
#define BBCLAW_ES8311_DAC_OSR 0x10
#endif

/*
 * BBCLAW_ES8311_BCLK_DIV — BCLK divider, register 0x06.
 * For 16-bit stereo I2S at 16 kHz with 4.096 MHz MCLK:
 *   BCLK = MCLK / (BCLK_DIV + 1) = 4.096 MHz / 4 = 1.024 MHz
 *   LRCK = BCLK / (2 × 16 bits) = 1.024 MHz / 32 = 32 kHz  ← wrong
 * Correct: BCLK must be 16 kHz × 32 = 512 kHz → BCLK_DIV = 7 (÷8).
 *   4.096 MHz / 8 = 512 kHz; 512 kHz / 32 = 16 kHz ✓
 * The early bring-up value of 0x03 (÷4 → 1.024 MHz BCLK) worked because
 * the I2S master (ESP32-S3) drives BCLK independently; the ES8311 in slave
 * mode accepts whatever BCLK the master provides. 0x07 is the datasheet-
 * recommended value for this MCLK/sample-rate combination and is set here
 * for correctness; the ESP I2S master clock is unaffected.
 */
#ifndef BBCLAW_ES8311_BCLK_DIV
#define BBCLAW_ES8311_BCLK_DIV 0x07
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

/* PSRAM 全帧渲染 + 内部 DMA 条带搬运(见 bb_lvgl_display.c canvas 路径)。
 * 消掉小条带导致的 CJK 重复排版;板级按内存/面板情况开启。 */
#ifndef BBCLAW_LVGL_CANVAS_PSRAM
#define BBCLAW_LVGL_CANVAS_PSRAM 0
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
/* Default 0: byte-swapping is delegated to esp_lvgl_port via flags.swap_bytes.
 * Set to 1 only if the board's panel io layer must also swap (would double-swap
 * when used together with flags.swap_bytes — avoid unless intentional). */
#define BBCLAW_ST7789_SWAP_BYTES 0
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
