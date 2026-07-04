# Waveshare ESP32-S3-Touch-AMOLED-2.06 硬件参考

> BBClaw 拓展板适配目标 #1（已购，可直接使用）。
> 本文是官方 wiki + 官方 BSP 源码的整理归档，作为 BBClaw 板级适配的依据。
> 板级配置见 `firmware/boards/waveshare-amoled-206/`，适配决策见
> [ADR-040](../decisions/ADR-040-waveshare-extension-boards.md)。

## 资料来源

| 资料 | 链接 |
|------|------|
| 官方 Wiki（中文） | https://www.waveshare.net/wiki/ESP32-S3-Touch-AMOLED-2.06 |
| 官方示例仓库 | https://github.com/waveshareteam/ESP32-S3-Touch-AMOLED-2.06 |
| 官方 BSP 组件（引脚真相来源） | ESP Registry `waveshare/esp32_s3_touch_amoled_2_06` v2.0.0 |
| BSP 源码 | https://github.com/waveshareteam/Waveshare-ESP32-components → `bsp/esp32_s3_touch_amoled_2_06/` |
| 屏幕驱动组件 | ESP Registry `waveshare/esp_lcd_sh8601`（CO5300 兼容 SH8601 指令集） |
| 触摸驱动组件 | ESP Registry `esp_lcd_touch_ft5x06`（FT3168 兼容 FT5x06 协议） |

## 规格总表

| 项目 | 规格 |
|------|------|
| 主控 | ESP32-S3R8（Xtensa LX7 双核 240MHz，叠封 8MB **Octal** PSRAM） |
| Flash | wiki 标称 32MB；官方 demo sdkconfig 配 `FLASHSIZE_16MB` + QIO（待 `esptool flash_id` 实测确认） |
| 屏幕 | 2.06" AMOLED，410×502，16.7M 色，600nit，驱动 IC **CO5300**，QSPI 接口 |
| 触摸 | **FT3168** 电容触控，I2C（FT5x06 协议兼容） |
| 音频 codec | **ES8311**（I2C 控制 + I2S 数据），板载麦克风 + 板载扬声器（功放使能 GPIO46） |
| PMIC | **AXP2101**（I2C 0x34），MX1.25 锂电池口，建议 400mAh |
| IMU | QMI8658 六轴（I2C） |
| RTC | PCF85063（I2C） |
| SD 卡 | Micro SD，SDMMC 1-bit |
| USB | Type-C（原生 USB，烧录/调试） |
| 按键 | BOOT = GPIO0（低有效）；PWR = GPIO10（高有效，AXP2101 电源键路由） |
| 无线 | WiFi 2.4G 802.11 b/g/n + BLE5，板载天线 |
| 引出 | 1×I2C、1×UART、USB 焊盘 |

## GPIO 分配（来自官方 BSP 头文件，真相来源）

来源：`bsp/esp32_s3_touch_amoled_2_06/include/bsp/esp32_s3_touch_amoled_2_06.h`

| 外设 | 信号 | GPIO |
|------|------|------|
| I2C 总线（触摸/PMIC/IMU/RTC/codec 共享） | SCL | 14 |
| | SDA | 15 |
| AMOLED（QSPI） | CS | 12 |
| | PCLK(SCK) | 11 |
| | DATA0 | 4 |
| | DATA1 | 5 |
| | DATA2 | 6 |
| | DATA3 | 7 |
| | RST | 8 |
| | 背光 | 无（亮度走 CO5300 `0x51` 命令） |
| 触摸 FT3168 | RST | 9 |
| | INT | 38 |
| I2S（ES8311） | MCLK | 16 |
| | BCLK(SCLK) | 41 |
| | WS(LRCK) | 45 |
| | DOUT（ESP→codec DAC） | 40 |
| | DIN（codec ADC→ESP） | 42 |
| 功放使能 PA_EN | | 46（高有效） |
| SD 卡（SDMMC 1-bit） | CLK | 2 |
| | CMD | 1 |
| | D0 | 3 |
| | （SPI 模式备选 CS，见 wiki） | 17 |
| BOOT 键 | | 0（低=按下） |
| PWR 键 | | 10（高=按下） |

I2C 设备地址：AXP2101=0x34，ES8311=0x18，FT3168=0x38（FT5x06 默认），QMI8658=0x6B/0x6A，PCF85063=0x51。

## 关键怪癖（来自 BSP 源码，适配必读）

1. **亮度无背光 GPIO**：AMOLED 亮度通过面板命令 `0x51 <0x00–0xFF>` 设置（QSPI
   编帧 `0x02<<24 | 0x51<<8`）。BSP 初始化后默认打满 0xFF。
2. **刷新区域必须 2 像素对齐**：CO5300 要求 x/y 起点向下取偶、终点取奇。LVGL9 下
   注册 `LV_EVENT_INVALIDATE_AREA` rounder 回调（BSP `rounder_event_cb`）。
3. **面板 X gap = 0x16（22px）**：`esp_lcd_panel_set_gap(panel, 0x16, 0)`；列地址
   实际范围 0x16–0x1AF（即 22..431 共 410 列）。
4. **CO5300 init 序列**（BSP `lcd_init_cmds[]`）：`0x11`(sleep out, delay120) →
   `0xC4 0x80` → `0x44 0x01D1`(TE line) → `0x35 0x00`(TE on) → `0x53 0x20` →
   `0x63 0xFF` → `0x51 0x00` → `0x2A/0x2B` 窗口 → `0x29`(disp on) → `0x51 0xFF`。
5. **AXP2101 免初始化可点屏**：BSP 完全不碰 PMU，说明 AMOLED/codec 供电轨默认
   上电即开。PMU 驱动只在需要电量/充电状态/电源键事件时才要（适配分期落后做）。
   官方 `01_AXP2101` 示例用 XPowersLib，充电参数：预充 50mA、恒流 400mA、截止
   25mA、4.2V；**必须 `disableTSPinMeasure()`**（板上无电池 NTC，不关会充电异常）。
6. **麦克风路径存疑**：wiki 说 ES8311 单 codec 收+放；但 BSP 的
   `bsp_audio_codec_microphone_init()` 却初始化 ES7210（疑似从 ESP-SparkBot BSP
   复制的残留，文件内注释也写着 "ESP-SparkBot-BSP pinout"）。BBClaw 按 ES8311
   duplex 适配；若实测收音失败，用 I2C 扫描确认 0x40（ES7210）是否在板。
7. **LVGL 缓冲**：BSP 用 `H_RES × 50` 行、单缓冲、PSRAM、`swap_bytes=true`、
   `sw_rotate`、非 DMA。全屏 410×502×2B ≈ 402KB。
8. **官方 demo sdkconfig 要点**：QIO flash、`SPIRAM_MODE_OCT`、`SPIRAM_SPEED_80M`、
   `SPIRAM_FETCH_INSTRUCTIONS/RODATA`、`ESP32S3_DATA_CACHE_LINE_64B`、CPU 240MHz。

## BBClaw 适配映射

| BBClaw 子系统 | 本板落点 |
|---------------|----------|
| 显示 `bb_panel` | 新增 QSPI 总线路径 + SH8601 驱动（`BBCLAW_DISPLAY_BUS_QSPI`） |
| 音频 `bb_audio` | `BBCLAW_AUDIO_INPUT_SOURCE "es8311"`（既有 ES8311 代码路径首次真机启用） |
| PTT | BOOT 键 GPIO0（低有效）——板上无侧键/滚轮 |
| 导航 | 无实体导航键；触摸（FT3168 → LVGL indev）为后续阶段 |
| 电量 `bb_power` | ADC 分压不可用；需走 AXP2101 I2C（后续阶段，先禁用） |
| 状态灯 | 无板载 LED，禁用 |
| 马达 | 无，禁用 |

## 与 BBClaw 正式板（bbclaw PCB）的差异提醒

- PSRAM 是 **Octal**（bbclaw 量产板同为 OCT，breadboard/dev 默认 QUAD）——
  切板时 `SPIRAM_MODE_OCT` 必须跟着切，否则 `wrong PSRAM line mode` boot loop。
- 分辨率 410×502 竖屏 vs bbclaw 320×172 横屏，UI 布局需要另行调优。
- 无导航滚轮/侧键：现有按键导航交互在此板上仅剩 BOOT 单键 + 触摸。
