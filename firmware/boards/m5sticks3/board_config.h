/**
 * BBClaw board config: M5Stack M5StickS3 (ESP32-S3-PICO-1-N8R8)
 *
 * 硬件参考（引脚真相来源 = M5Stack 官方 pinmap + M5GFX/M5Unified board_M5StickS3
 *   源码,双源交叉验证,高置信）:
 *   design/boards/m5sticks3-esp32s3.md  (待补)
 *
 * ESP32-S3-PICO-1-N8R8（8MB flash + 8MB Octal PSRAM）+ 1.14" SPI ST7789P3
 * 135x240 + ES8311 codec（收放一体:DAC 出喇叭 / MEMS mic 走 ES8311 自带 ADC,
 * 无 ES7210）+ AW8737 功放 + M5PM1 自研 PMIC（I2C 0x6E）+ BMI270 IMU + 红外收发。
 *
 * ⚠️ 电源在 M5PM1 后面(非 AXP):功放使能(PYG3)、电量、可能连屏/mic/喇叭的
 *    L3B 供电轨都要经 M5PM1 I2C 写寄存器才通。bbclaw 暂无 M5PM1 驱动 ——
 *    本 header 只落引脚/显示/音频,M5PM1 上桥(bb_audio_set_pa_control 钩子 +
 *    boot 时 enable L3B)是真机 bring-up 阶段要补的唯一新代码。见 §开放问题。
 */
#pragma once

/* ── OTA identity: 独立平台名,云端无此平台 release → 不会被推 bbclaw 正式板固件 ── */
#define BBCLAW_OTA_PLATFORM "esp32s3-m5sticks3"

/* ================= Display: ST7789P3 1.14" 135x240 over SPI ================= */
#define BBCLAW_DISPLAY_BUS_SPI 1
#define BBCLAW_DISPLAY_BUS_I80 0

#define BBCLAW_ST7789_HOST      2   /* 与 lichuang 同用 SPI3(host 值 2) */
#define BBCLAW_ST7789_SCLK_GPIO 40
#define BBCLAW_ST7789_MOSI_GPIO 39
#define BBCLAW_ST7789_DC_GPIO   45
#define BBCLAW_ST7789_CS_GPIO   41
#define BBCLAW_ST7789_RST_GPIO  21
#define BBCLAW_ST7789_BL_GPIO   38

/* 面板原生竖屏 135(W)x240(H),rotation0 MADCTL=0x00。1.14" ST7789 有非零行列
 * 偏移:M5GFX board_M5StickS3 给的是 offset_x=52 / offset_y=40。 */
#define BBCLAW_ST7789_WIDTH   135
#define BBCLAW_ST7789_HEIGHT  240
#define BBCLAW_ST7789_X_GAP   52
#define BBCLAW_ST7789_Y_GAP   40

#define BBCLAW_ST7789_SWAP_XY       0   /* 原生竖屏,不旋 */
#define BBCLAW_ST7789_MIRROR_X      0   /* TODO 真机核对镜像 */
#define BBCLAW_ST7789_MIRROR_Y      0   /* TODO 真机核对镜像 */
#define BBCLAW_ST7789_INVERT_COLOR  1   /* M5GFX invert=true → INVON(0x21) */
#define BBCLAW_ST7789_RGB_ORDER_BGR 0   /* RGB;若红蓝对调再翻 BGR */
#define BBCLAW_ST7789_SWAP_BYTES    0   /* 同 lichuang esp_lcd 路径起手 0;花屏再试 1 */
#define BBCLAW_ST7789_PCLK_HZ      (40 * 1000 * 1000)
#define BBCLAW_ST7789_SPI_MODE     0    /* M5GFX StickS3 用 SPI mode 0(≠lichuang 的 mode2) */

/* 背光 GPIO38 直驱高有效即可点亮(M5GFX 用 PWM 仅为调光;on/off 拉高就行)。
 * 起手用普通 GPIO 电平,若不亮再试 BL_PWM=1(参考 lichuang 升压面板需 PWM 的坑)。 */
#define BBCLAW_ST7789_BL_ACTIVE_LEVEL 1
/* BBCLAW_ST7789_BL_PWM 保持默认 0(普通电平背光) */

/* ================= Audio: ES8311 收放一体 + AW8737 PA ================= */
#define BBCLAW_AUDIO_INPUT_SOURCE "es8311"   /* 覆盖默认 "inmp441" */
#define BBCLAW_AUDIO_SAMPLE_RATE  16000

#define BBCLAW_AUDIO_I2S_MCK_GPIO 18   /* ES8311 需要 MCLK */
#define BBCLAW_AUDIO_I2S_BCK_GPIO 17
#define BBCLAW_AUDIO_I2S_WS_GPIO  15
#define BBCLAW_AUDIO_I2S_DO_GPIO  14   /* ESP TX → ES8311 DAC → 喇叭 */
#define BBCLAW_AUDIO_I2S_DI_GPIO  16   /* ES8311 ADC(mic) → ESP RX */

/* mic 走 ES8311 自带 ADC,不用 ES7210(保持默认 0)。 */
/* #define BBCLAW_ES7210_ENABLE 0  (默认即 0,显式留注释) */

/* I2C 总线(ES8311 0x18 / BMI270 0x68 / M5PM1 0x6E 共享) */
#define BBCLAW_ES8311_I2C_PORT     0
#define BBCLAW_ES8311_I2C_SDA_GPIO 47
#define BBCLAW_ES8311_I2C_SCL_GPIO 48
#define BBCLAW_ES8311_I2C_ADDR     0x18

/* 功放(AW8737)使能 = M5PM1 PYG3,经 I2C 非 ESP GPIO。
 * → GPIO 留 -1,真机 bring-up 时注册 bb_audio_set_pa_control() 钩子写 M5PM1
 *   开/关功放(仿 bb_radio_app.c 的 pca9557_pa_ctrl)。TODO M5PM1 驱动。 */
#define BBCLAW_PA_EN_GPIO      -1
#define BBCLAW_SPEAKER_SW_GPIO -1   /* 无硬件 mute-switch 输入;必须覆盖全局默认 1 */

/* M5PM1 PMIC 最小上电:开 L3B 轨(屏/mic/喇叭电源)+ 注册功放门控钩子(GPIO3)。
 * 跑在 bb_audio_init 探 ES8311 之前——不开 L3B 则 codec 无电探不到 → boot loop。 */
#define BBCLAW_M5PM1_MINIMAL_INIT 1

/* ================= Buttons / PTT ================= */
#define BBCLAW_PTT_GPIO         11   /* 前面板按键 A;TODO 真机核对 A/B 与有效电平 */
#define BBCLAW_PTT_ACTIVE_LEVEL 0    /* 假定按下接地,内部上拉 */
#define BBCLAW_PTT_PULL_UP      1
/* ── Navigation: 侧键 GPIO12 = 单键 OK(短按)/长按 BACK。走 Flipper 6-button
 *    路径但只接 OK 键,其余 up/down/left/right/back 保持默认 -1 不接(poll_btn 自动
 *    no-op)。长按阈值 = BBCLAW_NAV_LONG_PRESS_MS(默认 700ms)。 ── */
#define BBCLAW_NAV_ENABLE           1
#define BBCLAW_NAV_FLIPPER_6BUTTON  1
#define BBCLAW_NAV_BTN_OK_GPIO      12
#define BBCLAW_NAV_KEY_ACTIVE_LEVEL 0   /* 按下接地,内部上拉 */
#define BBCLAW_NAV_PULL_UP          1

/* ================= 关掉本板没有的外设(否则默认值会抢引脚) ================= */
/* 马达默认开且 GPIO=21,正好撞 LCD RST → 必须关 */
#define BBCLAW_MOTOR_ENABLE 0
#define BBCLAW_MOTOR_GPIO   -1
/* RYG 状态灯默认开且抢 GPIO 2/4/5 → 关 */
#define BBCLAW_STATUS_LED_ENABLE 0
/* 电量/充电经 M5PM1 I2C（无 ESP ADC 脚）：VBAT reg0x22/23、电源来源 reg0x04。
 * 见 bb_power.c 的 M5PM1 后端。250mAh 电池,满/空电压用全局默认 4200/3300mV。 */
#define BBCLAW_POWER_ENABLE       1
#define BBCLAW_POWER_SOURCE_M5PM1 1
#define BBCLAW_POWER_ADC_GPIO     -1

/* ── IMU: BMI270 @0x68。用于「拿起唤醒 / 静止息屏」电源管理(接入 bb_sleep_manager)。
 *    倾斜导航(BBCLAW_IMU_BMI270_NAV)实测难调、非行业主流,默认关,留作实验开关;
 *    导航交给实体键(PTT + 侧键)。QMI8658 那套(BBCLAW_IMU_ENABLE)型号不同,保持关。 ── */
#define BBCLAW_IMU_ENABLE      0
#define BBCLAW_IMU_BMI270_NAV  0
#define BBCLAW_IMU_BMI270_WAKE 1

/* ── 息屏:关背光;BMI270 拿起唤醒 + 按键/消息唤醒。250mAh 小电池宜短超时省电。
 *    息屏时长走 preset(设置里可改并存 NVS);默认档 30s(index 1)。变暗 15s。 ── */
#define BBCLAW_DISPLAY_BRIGHTNESS_CONTROL 1
#define BBCLAW_SLEEP_DIMMING_TIMEOUT_MS  (15 * 1000) /* 15s 变暗 */
#define BBCLAW_SLEEP_PRESET_DEFAULT_IDX  1           /* 电池模式:息屏 preset 默认 30s */
/* 充电模式:插 USB 时不 DISPOFF,停在暗屏桌面时钟(时间+电量+充电),拔了回省电息屏。 */
#define BBCLAW_CHARGING_AMBIENT_CLOCK    1

/* 无 camera / PCA9557 / XL9555 / touch / SD 卡。 */
