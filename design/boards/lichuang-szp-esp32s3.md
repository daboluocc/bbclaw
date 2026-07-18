# 嘉立创·实战派 ESP32-S3 开发板（lichuang-szp）适配

状态: 第一阶段 bring-up（显示 + ES8311/ES7210 收放音 + BOOT 键 PTT + FT6336 触摸）
日期: 2026-07-18

## 硬件概况

嘉立创（立创开发板）「实战派」ESP32-S3 开发板：

- **SoC/模组**: ESP32-S3-WROOM-1 N16R8 — 16MB flash + **8MB Octal PSRAM**
  （⚠ 与 sdkconfig.defaults 的 QUAD 不同，必须 `CONFIG_SPIRAM_MODE_OCT`，
  否则 `wrong PSRAM line mode` boot loop，与 waveshare 手表板同坑）
- **显示**: 2.0" SPI ST7789 320x240（横屏 swap_xy + mirror_x）
- **触摸**: FT6336（FT5x06 协议兼容，I2C 0x38，INT/RST 未接 GPIO）
- **音频**: ES8311 codec（DAC 放音）+ ES7210 ADC（双 mic 收音），共享一条 I2S
- **IO 扩展**: PCA9557（I2C 0x19）— bit0=LCD_CS、bit1=PA_EN（功放）、bit2=摄像头 PWDN
- **摄像头**: OV2640 DVP（本阶段不适配）
- **LED**: GPIO48 WS2812（bb_led 仅支持三线 PWM，暂关）

引脚真相来源: xiaozhi-esp32 `main/boards/lichuang-dev/`（config.h + lichuang_dev_board.cc）。

## 引脚表

| 功能 | GPIO |
|------|------|
| I2C SDA / SCL（ES8311/ES7210/PCA9557/FT6336 共享，port 0） | 1 / 2 |
| I2S MCLK / BCLK / WS | 38 / 14 / 13 |
| I2S DOUT（ESP→ES8311 DAC）/ DIN（ES7210→ESP） | 45 / 12 |
| LCD SPI MOSI / SCLK / DC | 40 / 41 / 39 |
| LCD CS / RST | PCA9557 bit0（拉低常选通）/ 未接 |
| LCD 背光 | 42，**低电平点亮（反相）** |
| PA 功放使能 | PCA9557 bit1，高有效 |
| BOOT 键（= PTT） | 0，低有效 |

## 适配决策

1. **PCA9557 最小驱动**（`bb_pca9557.c`，仿 `bb_xl9555.c`）：init 时 bit0/bit1 配输出、
   LCD_CS=0（唯一 SPI 设备，常选通）、PA=0；由 `bb_panel.c` SPI 路径在建 panel io 前调用
   （保证首条 LCD 命令前 CS 已有效）。功放在 radio app 音频 init 后置 1 常开（同 ATK
   板 XL9555 先例）。
2. **背光反相**: 新增 `BBCLAW_ST7789_BL_ACTIVE_LEVEL`（默认 1，旧板零变化），
   本板设 0。覆盖 bb_display_bitmap / bb_lvgl_display / bb_display_control_st7789 三处。
2b. **SPI mode 2（真机黑屏坑）**: 本板面板要求 CPOL=1 的 SPI mode 2；bb_panel 原
   硬编码 mode 0，首刷软件层全"正常"（panel ready / LVGL 渲染 / 截图都好）但物理屏
   全黑。新增 `BBCLAW_ST7789_SPI_MODE`（默认 0，本板 2）。真相来源 = xiaozhi
   lichuang_dev_board.cc `io_config.spi_mode = 2`。
3. **I2C get-or-create**: 显示 init（PCA9557）先于 `bb_audio_init` 建 port0 总线，
   es8311 路径由无条件 `i2c_new_master_bus` 改为先 `i2c_master_get_bus_handle`
   （与 xl9555 同模式），顺序无关化。其他板行为不变（无人先建时仍走新建）。
4. **喇叭使能脚**: `BBCLAW_SPEAKER_SW_GPIO` 全局默认是 GPIO1，本板 GPIO1 = I2C SDA，
   必须显式 -1。
5. **OTA 平台名**: `esp32s3-lichuang-szp`，云端无此平台 release → 不会被误推 bbclaw 固件。
6. 导航无实体键：`BBCLAW_NAV_ENABLE 0`，触摸 indev + 手势（右滑 BACK/左滑 LEFT）
   沿用手表板路径；PTT= BOOT 键。

## 构建

```bash
make set-board BOARD=lichuang-szp && make build
# 或隔离构建（不动主 build/）：
idf.py -B build-lichuang -DSDKCONFIG=build-lichuang/sdkconfig \
  -DSDKCONFIG_DEFAULTS="sdkconfig.defaults;boards/lichuang-szp/sdkconfig.board" build
```
