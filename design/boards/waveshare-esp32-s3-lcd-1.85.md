# Waveshare ESP32-S3-LCD-1.85 硬件参考（无触摸版本）

> BBClaw 拓展板适配目标 #2（已购，可直接使用）。适配排在 AMOLED-2.06 之后。
> 本文整理自官方 wiki；引脚表以 wiki 为准，动手适配前建议再对一遍官方 demo /
> 原理图（本板尚未做 BSP 级核对）。适配决策见
> [ADR-040](../decisions/ADR-040-waveshare-extension-boards.md)。

## 资料来源

| 资料 | 链接 |
|------|------|
| 官方 Wiki（中文） | https://www.waveshare.net/wiki/ESP32-S3-LCD-1.85 |
| 示例程序包 | https://files.waveshare.net/wiki/ESP32-S3-LCD-1.85/ESP32-S3-LCD-1.85-Demo.zip |
| 原理图 | https://www.waveshare.net/w/upload/7/76/ESP32-S3-LCD-1.85.pdf |

## 规格总表

| 项目 | 规格 |
|------|------|
| 主控 | ESP32-S3R8（双核 240MHz，8MB **Octal** PSRAM） |
| Flash | 16MB |
| 屏幕 | 1.85" TFT LCD，360×360，驱动 IC **ST77916**，QSPI 接口，背光 GPIO 调光 |
| 触摸 | 我们购买的是**无触摸版本**（有触摸版为 I2C + 中断） |
| 音频输出 | **PCM5101** I2S DAC（无 I2C 控制寄存器）+ 板载功放，8Ω 2W 2030 喇叭接口 |
| 音频输入 | **ICS-43434** I2S 数字麦克风（新版；旧版 MSM261S4030H0R） |
| 电源 | 锂电池充电管理芯片 + ME6217C33M5G LDO（800mA），MX1.25 3.7V 电池口 |
| GPIO 扩展 | **TCA9554PWR**（I2C，EXIO0–7，SD 卡 CS 等挂在扩展口上） |
| IMU | QMI8658 六轴（I2C） |
| RTC | PCF85063（I2C，INT=GPIO9） |
| SD 卡 | Micro SD（SPI 模式，CS 走 TCA9554 EXIO3） |
| USB | Type-C |
| 天线 | 贴片陶瓷天线，可切外置 |

## GPIO 分配（来自官方 wiki）

| 外设 | 信号 | GPIO |
|------|------|------|
| LCD（QSPI ST77916） | SDA0 | 46 |
| | SDA1 | 45 |
| | SDA2 | 42 |
| | SDA3 | 41 |
| | SCK | 40 |
| | CS | 21 |
| | TE | 18 |
| | 背光 BL | 5 |
| | RST | 未在 wiki 表中列出（可能走 TCA9554 EXIO，看原理图） |
| I2C 总线（IMU/RTC/TCA9554 共享） | SCL | 10 |
| | SDA | 11 |
| RTC PCF85063 | INT | 9 |
| 麦克风 ICS-43434（I2S RX） | WS | 2 |
| | SCK | 15 |
| | SD | 39 |
| 扬声器 PCM5101（I2S TX） | DIN | 47 |
| | LRCK | 38 |
| | BCK | 48 |
| SD 卡（SPI） | D0/MISO | 16 |
| | CMD/MOSI | 17 |
| | SCK | 14 |
| | D3/CS | TCA9554 EXIO3 |

I2C 设备地址：TCA9554=0x20（默认），QMI8658=0x6B/0x6A，PCF85063=0x51。

## 与 AMOLED-2.06 的适配差异（提前踩点）

1. **收放是两组独立 I2S 引脚**（mic WS2/SCK15/SD39；spk DIN47/LRCK38/BCK48），
   不是共享 BCLK/WS 的 duplex。BBClaw `bb_audio` 目前按单 I2S duplex 建通道，
   适配本板需要支持 **TX/RX 各占一个 I2S 控制器**（S3 有 I2S0+I2S1，硬件够用）。
2. **无 codec 寄存器**：PCM5101 与 ICS-43434 都是纯 I2S 器件，走类似现有
   `inmp441` 路径（无 I2C 控制），不是 ES8311 路径。
3. **ST77916 vs CO5300**：同为 QSPI，但驱动组件不同（ESP Registry 有
   `esp_lcd_st77916`）。`bb_panel` 的 QSPI 总线路径可复用，换 panel driver 即可。
4. **背光是真 GPIO（5）**，与 AMOLED 的命令式亮度不同；接现有 BL GPIO 逻辑即可。
5. **TCA9554 扩展器**：SD CS、可能还有 LCD RST 挂在 EXIO 上——BBClaw 已有
   XL9555 驱动先例（`bb_xl9555.c`），TCA9554 寄存器模型类似（另写小驱动）。
6. 无触摸版本 + 无导航键：输入面比 AMOLED-2.06 更受限，交互设计要单独考虑
   （BOOT 键 + 语音为主）。
