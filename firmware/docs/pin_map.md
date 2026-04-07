# BBClaw 固件引脚映射（Pin Map）

更新时间：2026-03-24

本文档是固件接线的单一基线，包含信号线与供电线（`3V3` / `5V`）建议。

## 1. 当前默认配置（v0：INMP441 + MAX98357A）

对应配置宏（`firmware/include/bb_config.h`）：
- `BBCLAW_AUDIO_INPUT_SOURCE="inmp441"`
- `BBCLAW_PTT_GPIO=0`
- `BBCLAW_MOTOR_GPIO=21`
- `BBCLAW_STATUS_LED_R_GPIO=2`
- `BBCLAW_STATUS_LED_Y_GPIO=4`
- `BBCLAW_STATUS_LED_G_GPIO=5`

### 1.1 INMP441 麦克风接线（3.3V）

| 模块引脚 | 接到开发板 | 说明 |
| --- | --- | --- |
| `VDD` | `3V3` | 数字麦克风供电，使用 3.3V |
| `GND` | `GND` | 共地 |
| `SCK` | `GPIO16` | I2S `BCLK` |
| `WS` | `GPIO15` | I2S `WS/LRCK` |
| `SD` | `GPIO18` | I2S 数据输出（mic -> ESP） |
| `L/R` | `GND` 或 `3V3` | 左右声道选择；当前固件默认会自动锁定有能量的一侧 |

### 1.2 MAX98357A 功放接线（推荐 5V）

| 模块引脚 | 接到开发板 | 说明 |
| --- | --- | --- |
| `VIN` | `5V`（推荐） | 5V 输出功率更高；也可按模块规格用 3.3V |
| `GND` | `GND` | 共地 |
| `BCLK` | `GPIO16` | 与 INMP441 共享 I2S 时钟 |
| `LRC/LRCLK` | `GPIO15` | 与 INMP441 共享 I2S 字时钟 |
| `DIN` | `GPIO17` | I2S 播放数据（ESP -> amp） |
| `SD/EN`（若有） | 悬空或 GPIO | 默认常开；需要静音控制时再接 GPIO |
| `SPK+ / SPK-` | 扬声器正负极 | 按扬声器规格连接 |

### 1.3 状态灯接线（对讲状态）

在 **`idf.py menuconfig` → BBClaw → Status LED** 中选择接线方式：

- **Discrete R / Y / G**：三颗独立 LED（黄灯独占一脚）
- **RGB module**：共阴 RGB 小模块（`R` / `G` / `B` / `GND`，板载限流电阻，高电平点亮），见 `docs/led-RGB.png`

#### 独立 R / Y / G（默认）

| LED 引脚 | 接到开发板 | 说明 |
| --- | --- | --- |
| `R` | `GPIO2` | 红色通道，串 `100Ω` 限流电阻 |
| `Y` | `GPIO4` | 黄色通道，串 `100Ω` 限流电阻 |
| `G` | `GPIO5` | 绿色通道，串 `100Ω` 限流电阻 |
| `COM-` | `GND` | 共阴极，直接接地 |

menuconfig 中默认 **R/Y/G = GPIO 2 / 4 / 5**。

#### 共阴 RGB 模块（`led-RGB.png`）

| 模块丝印 | 接到开发板 | 说明 |
| --- | --- | --- |
| `R` | `GPIO2` | 红通道（默认，可改） |
| `G` | `GPIO4` | 绿通道（menuconfig 里为 Green channel GPIO） |
| `B` | `GPIO5` | 蓝通道 |
| `GND` | `GND` | 公共负极 |

固件现在使用 **LEDC PWM** 驱动状态灯，支持全局亮度控制；**「黄」语义**在 RGB 模式下用 **红+绿同时点亮**（`R+G`）模拟，与独立黄灯视觉一致。

默认亮度由 `BBCLAW_STATUS_LED_BRIGHTNESS_PCT` 控制，定义在 [bb_config.h](/Volumes/1TB/github/bbclaw/firmware/include/bb_config.h)；若某块板子仍然偏亮，优先在对应 `boards/*/board_config.h` 中覆盖。

灯语与 `rgb_led_status.md` 对齐的简化版：

| 状态 | 独立 RYG | RGB 模块 |
| --- | --- | --- |
| 待机 | 绿常亮 | 绿常亮 |
| 录音 | 红常亮 | 红常亮 |
| 处理中（闪） | 黄闪 | 黄（R+G）闪 |
| 回复（闪） | 绿闪 | 绿闪 |
| 通知 / 错误（闪） | 红闪 | 红闪 |

开机跑马灯：RYG 为 **红→黄→绿**；RGB 模块为 **红→绿→蓝**。

### 1.4 振动马达接线（交互增强）

| 模块引脚 | 接到开发板 | 说明 |
| --- | --- | --- |
| `IN/SIG` | `GPIO21` | 控制信号（`BBCLAW_MOTOR_GPIO`） |
| `VCC` | `5V`（常见） | 按马达/驱动板额定电压供电 |
| `GND` | `GND` | 共地 |

注意：马达必须通过 NPN/MOSFET 或驱动板带载，不能直接由 ESP32-S3 GPIO 供电。

软件侧：PTT **按下**用 `BBCLAW_MOTOR_PULSE_SHORT_MS`（长一点的单震）、**松开**用 `BBCLAW_MOTOR_PULSE_RELEASE_MS`（更短），与按键“按下—回弹”成对；通知为双短震，错误为 `BBCLAW_MOTOR_PULSE_LONG_MS` 单长震（见 `bb_motor.h` / `bb_config.h`）。

### 1.5 ST7789 屏幕接线（按模块规格 3.3V/5V）

| 模块引脚 | 接到开发板 | 说明 |
| --- | --- | --- |
| `VCC` | `3V3`（优先） | 若模块明确支持 5V 再接 5V |
| `GND` | `GND` | 共地 |
| `SCLK` | `GPIO12` | SPI 时钟 |
| `MOSI` | `GPIO11` | SPI 数据 |
| `CS` | `GPIO10` | 片选 |
| `DC` | `GPIO9` | 数据/命令 |
| `RST` | `GPIO14` | 复位 |
| `BLK/BL` | `GPIO13` | 背光控制 |

### 1.6 PTT 按键接线

| 端子 | 接到开发板 | 说明 |
| --- | --- | --- |
| 按键一端 | `GPIO0` | `BBCLAW_PTT_GPIO`，低电平有效 |
| 按键另一端 | `GND` | 使用内部上拉输入 |

说明：
- 当前固件默认编译配置就是 `GPIO0`，不是 `GPIO7`。
- 若你的板子把 PTT 接到了别的脚，需要同步改 `BBCLAW_PTT_GPIO`。

## 2. ES8311 兼容模式（可选）

对应配置宏：
- `BBCLAW_AUDIO_INPUT_SOURCE="es8311"`

### 2.1 ES8311+NS4150B 模块接线（含供电）

| 模块引脚 | 接到开发板 | 说明 |
| --- | --- | --- |
| `SDA` | `GPIO8` | I2C 控制 |
| `SCL` | `GPIO6` | I2C 控制 |
| `MCK` | `GPIO2` | I2S 主时钟；若启用 RYG 状态灯，需改配 `BBCLAW_ES8311_I2S_MCK_GPIO` 或 LED 红灯 GPIO |
| `BCK` | `GPIO16` | I2S 位时钟 |
| `WS` | `GPIO15` | I2S 字时钟 |
| `DI` | `GPIO18` | I2S 输入（to ESP RX） |
| `DO` | `GPIO17` | I2S 输出（from ESP TX） |
| `3V3` | `3V3` | 数字域供电 |
| `VCC` | `5V`（待模块铭牌确认） | 功放域常用 5V |
| `GND` | `GND` | 共地 |

## 3. 供电与接线注意事项

- 所有模块必须共地（`GND` 共地）。
- 功放/马达优先走 `5V` 供电（按模块额定电压确认）。
- 麦克风数字模块（如 INMP441）使用 `3V3` 供电。
- `GPIO45/GPIO46` 避免作为外设上拉输入使用。
- Wi-Fi/BLE 同时启用时，`ADC2` 不用于 ADC 采样功能。
- RYG 状态灯默认占用 `GPIO2/4/5`；若切到 `ES8311` 模式，默认 `MCK=GPIO2` 会与红灯冲突，需要二选一重映射。
- `BBCLAW_PA_EN_PROBE_GPIO1` 当前默认也是 `GPIO13`，但 `BBCLAW_PA_EN_PROBE_ON_BOOT=0` 默认关闭；若启用开机功放探测，需要避免和马达脚复用。
- `INMP441` 默认开启 `BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK=1`，`L/R` 接 `GND` 或 `3V3` 都能自动选择非零声道。

## 4. 相关资料

- [硬件选型与资料缺口](./hardware_selection.md)
- [Adapter 对接说明](./bbclaw_adapter_integration.md)
- 模块图：
  - `INMP441.png`
  - `motor.png`
  - `ES8311+NS4150B.png`
