# BBClaw 固件引脚映射（Pin Map）

更新时间：2026-04-11

本文档是固件接线的单一基线，包含信号线与供电线（`3V3` / `5V`）建议。

## 1. Breadboard 默认配置（v0：INMP441 + MAX98357A）

对应配置宏（`firmware/include/bb_config.h`）：

- `BBCLAW_AUDIO_INPUT_SOURCE="inmp441"`
- `BBCLAW_PTT_GPIO=7`
- `BBCLAW_MOTOR_GPIO=21`
- `BBCLAW_STATUS_LED_R_GPIO=2`
- `BBCLAW_STATUS_LED_Y_GPIO=4`
- `BBCLAW_STATUS_LED_G_GPIO=5`

### 1.1 INMP441 麦克风接线（3.3V）


| 模块引脚  | 接到开发板         | 说明                       |
| ----- | ------------- | ------------------------ |
| `VDD` | `3V3`         | 数字麦克风供电，使用 3.3V          |
| `GND` | `GND`         | 共地                       |
| `SCK` | `GPIO16`      | I2S `BCLK`               |
| `WS`  | `GPIO15`      | I2S `WS/LRCK`            |
| `SD`  | `GPIO18`      | I2S 数据输出（mic -> ESP）     |
| `L/R` | `GND` 或 `3V3` | 左右声道选择；当前固件默认会自动锁定有能量的一侧 |

参考 [INMP441.png](./INMP441.png)：模块丝印一侧为 `L/R` / `WS` / `SCK`，另一侧为 `SD` / `VDD` / `GND`。接线时以模块丝印为准，不要把 `SD` 和 `WS/SCK` 看反。


### 1.2 MAX98357A 功放接线（推荐 5V）


| 模块引脚          | 接到开发板    | 说明                      |
| ------------- | -------- | ----------------------- |
| `VIN`         | `5V`（推荐） | 5V 输出功率更高；也可按模块规格用 3.3V |
| `GND`         | `GND`    | 共地                      |
| `BCLK`        | `GPIO16` | 与 INMP441 共享 I2S 时钟     |
| `LRC/LRCLK`   | `GPIO15` | 与 INMP441 共享 I2S 字时钟    |
| `DIN`         | `GPIO17` | I2S 播放数据（ESP -> amp）    |
| `SD/EN`（若有）   | 悬空或 GPIO | 默认常开；需要静音控制时再接 GPIO     |
| `SPK+ / SPK-` | 扬声器正负极   | 按扬声器规格连接                |


### 1.3 状态灯接线（对讲状态）

在 `**idf.py menuconfig` → BBClaw → Status LED** 中选择接线方式：

- **Discrete R / Y / G**：三颗独立 LED（黄灯独占一脚）
- **RGB module**：共阴 RGB 小模块（`R` / `G` / `B` / `GND`，板载限流电阻，高电平点亮），见 `docs/led-RGB.png`

#### 独立 R / Y / G（默认）


| LED 引脚 | 接到开发板   | 说明                 |
| ------ | ------- | ------------------ |
| `R`    | `GPIO2` | 红色通道，串 `100Ω` 限流电阻 |
| `Y`    | `GPIO4` | 黄色通道，串 `100Ω` 限流电阻 |
| `G`    | `GPIO5` | 绿色通道，串 `100Ω` 限流电阻 |
| `COM-` | `GND`   | 共阴极，直接接地           |


menuconfig 中默认 **R/Y/G = GPIO 2 / 4 / 5**。

#### 共阴 RGB 模块（`led-RGB.png`）


| 模块丝印  | 接到开发板   | 说明                                    |
| ----- | ------- | ------------------------------------- |
| `R`   | `GPIO2` | 红通道（默认，可改）                            |
| `G`   | `GPIO4` | 绿通道（menuconfig 里为 Green channel GPIO） |
| `B`   | `GPIO5` | 蓝通道                                   |
| `GND` | `GND`   | 公共负极                                  |


固件现在使用 **LEDC PWM** 驱动状态灯，支持全局亮度控制；**「黄」语义**在 RGB 模式下用 **红+绿同时点亮**（`R+G`）模拟，与独立黄灯视觉一致。

默认亮度由 `BBCLAW_STATUS_LED_BRIGHTNESS_PCT` 控制，定义在 [bb_config.h](/Volumes/1TB/github/bbclaw/firmware/include/bb_config.h)；若某块板子仍然偏亮，优先在对应 `boards/*/board_config.h` 中覆盖。

灯语与 `rgb_led_status.md` 对齐的简化版：


| 状态         | 独立 RYG | RGB 模块  |
| ---------- | ------ | ------- |
| 待机         | 绿常亮    | 绿常亮     |
| 录音         | 红常亮    | 红常亮     |
| 处理中（闪）     | 黄闪     | 黄（R+G）闪 |
| 回复（闪）      | 绿闪     | 绿闪      |
| 通知 / 错误（闪） | 红闪     | 红闪      |


开机跑马灯：RYG 为 **红→黄→绿**；RGB 模块为 **红→绿→蓝**。

### 1.4 振动马达接线（交互增强）


| 模块引脚     | 接到开发板    | 说明                        |
| -------- | -------- | ------------------------- |
| `IN/SIG` | `GPIO21` | 控制信号（`BBCLAW_MOTOR_GPIO`） |
| `VCC`    | `5V`（常见） | 按马达/驱动板额定电压供电             |
| `GND`    | `GND`    | 共地                        |


注意：马达必须通过 NPN/MOSFET 或驱动板带载，不能直接由 ESP32-S3 GPIO 供电。

软件侧：PTT **按下**用 `BBCLAW_MOTOR_PULSE_SHORT_MS`（长一点的单震）、**松开**用 `BBCLAW_MOTOR_PULSE_RELEASE_MS`（更短），与按键“按下—回弹”成对；通知为双短震，错误为 `BBCLAW_MOTOR_PULSE_LONG_MS` 单长震（见 `bb_motor.h` / `bb_config.h`）。

### 1.5 ST7789 屏幕接线（按模块规格 3.3V/5V）


| 模块引脚     | 接到开发板     | 说明               |
| -------- | --------- | ---------------- |
| `VCC`    | `3V3`（优先） | 若模块明确支持 5V 再接 5V |
| `GND`    | `GND`     | 共地               |
| `SCLK`   | `GPIO12`  | SPI 时钟           |
| `MOSI`   | `GPIO11`  | SPI 数据           |
| `CS`     | `GPIO10`  | 片选               |
| `DC`     | `GPIO9`   | 数据/命令            |
| `RST`    | `GPIO14`  | 复位               |
| `BLK/BL` | `GPIO13`  | 背光控制             |


### 1.6 PTT 按键接线


| 端子    | 接到开发板   | 说明                             |
| ----- | ------- | ------------------------------ |
| 按键一端  | `GPIO7` | `BBCLAW_PTT_GPIO`，高电平有效        |
| 按键另一端 | `3V3`   | 当前 breadboard 默认无内部上拉，按下时输入高电平 |


说明：

- 当前 breadboard 默认编译配置为 `GPIO7`。
- 若你的板子把 PTT 接到了别的脚，需要同步改 `BBCLAW_PTT_GPIO`。

## 2. BBClaw 自研 PCB 扩展配置

在 `bbclaw` 板型上，音频为 **外接 INMP441 + MAX98357A**（**未使用 ES8311**）；并保留屏幕 / PTT / 马达与 breadboard 同类接法，另增加电池采样与拨轮开关。

对应配置宏（`firmware/boards/bbclaw/board_config.h`）：

- `BBCLAW_POWER_ENABLE=1`
- `BBCLAW_POWER_ADC_GPIO=3`
- `BBCLAW_NAV_ENABLE=1`
- `BBCLAW_NAV_ENC_A_GPIO=6`
- `BBCLAW_NAV_ENC_B_GPIO=8`
- `BBCLAW_NAV_KEY_GPIO=1`

### 2.0 BBClaw PCB（U7）与 breadboard 差异

原理图（MCU 符号 U7）与早期 breadboard 接线对齐关系如下；以 `firmware/boards/bbclaw/board_config.h` 为准。

**ST7789（IO9–14 连续）**


| 屏引脚    | GPIO     |
| ------ | -------- |
| `SCLK` | `GPIO9`  |
| `MOSI` | `GPIO10` |
| `RES`  | `GPIO11` |
| `DC`   | `GPIO12` |
| `CS`   | `GPIO13` |
| `BLK`  | `GPIO14` |


**I2S（INMP441 + MAX98357A）**


| 功能                  | GPIO                                    | 说明                                                                                                    |
| ------------------- | --------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `WS` / `LRCK`       | `GPIO15`                                | 与 breadboard 同                                                                                        |
| `BCLK`              | `GPIO16`                                | 同                                                                                                     |
| `DOUT`→功放           | `GPIO17`                                | 同                                                                                                     |
| 麦克风 `SD`→ESP I2S RX | `GPIO20`（宏 `BBCLAW_AUDIO_I2S_DI_GPIO`） | **仅 INMP441**：模块丝印 **SD** → MCU I2S 收。须接实线；若量产 GPIO 不是 20，改宏与 PCB 一致。 |
| `MCLK`              | （未引出）                                   | 原理图 IO2 NC；`BBCLAW_AUDIO_I2S_MCK_GPIO=-1`，inmp441 路径不输出 MCLK                                         |


**状态 RGB（U6：单线可寻址灯珠，DIN）**


| 引脚     | 说明                                                                                                                          |
| ------ | --------------------------------------------------------------------------------------------------------------------------- |
| `DIN`  | 经 net「RGB1」至 `GPIO5`（与三线 PWM 共阴模块不同）                                                                                        |
| `DOUT` | 单颗灯时可 **悬空**（不级联下一颗）                                                                                                        |
| 固件     | `bb_led` 当前为 **LEDC 三线 PWM**；该硬件需 **RMT / led_strip** 等单线驱动。`board_config` 默认 `**BBCLAW_STATUS_LED_ENABLE=0`**，避免误用 PWM 脚位。 |


**其它**

- `GPIO4`：原理图为 **SD**（存储卡）；固件未启用 SD 时可仅占位。
- `GPIO41` / `GPIO42`：原理图 **KEY2** / **KEY1**；导航仍使用编码器 `6`/`8` + 按压 `GPIO1`，两枚独立按键尚未接软件逻辑。

### 2.1 电池电压采样（GPIO3 / ADC1_CH2）


| 节点          | 接到开发板                 | 说明                                 |
| ----------- | --------------------- | ---------------------------------- |
| `POWER_ADC` | `GPIO3`               | `BBCLAW_POWER_ADC_GPIO`，`ADC1_CH2` |
| 分压上臂 `Rtop` | `VBAT` -> `POWER_ADC` | 默认 `100kΩ`                         |
| 分压下臂 `Rbot` | `POWER_ADC` -> `GND`  | 默认 `100kΩ`                         |
| 滤波电容 `C`    | `POWER_ADC` -> `GND`  | 默认 `100nF`                         |


默认参数（`board_config.h`）：

- `BBCLAW_POWER_ENABLE=1`
- `BBCLAW_POWER_ADC_GPIO=3`
- `BBCLAW_POWER_ADC_RTOP_OHM=100000`
- `BBCLAW_POWER_ADC_RBOT_OHM=100000`
- `BBCLAW_POWER_BATTERY_EMPTY_MV=3300`
- `BBCLAW_POWER_BATTERY_FULL_MV=4200`

设计说明：

- 单节锂电池 `VBAT` 不能直接进 ESP32-S3 ADC，必须经分压网络。
- `100k / 100k` 时：
  - `4.2V -> 2.1V`
  - `3.3V -> 1.65V`
- 当前固件第一版仅做电压采样、电量换算与状态栏显示。
- 后续版本再叠加充电状态、低功耗策略与休眠逻辑。

### 2.2 拨轮开关 / 导航编码器接线（WS-003 类）

默认 `bbclaw` PCB 接入采用 `旋转编码器 + 按压` 方式，供后续的上翻 / 下翻 / 确认 / 长按扩展使用。


| 器件功能脚         | 接到开发板   | 说明                              |
| ------------- | ------- | ------------------------------- |
| `A`           | `GPIO6` | `BBCLAW_NAV_ENC_A_GPIO`，编码器 A 相 |
| `B`           | `GPIO8` | `BBCLAW_NAV_ENC_B_GPIO`，编码器 B 相 |
| `KEY`         | `GPIO1` | `BBCLAW_NAV_KEY_GPIO`，按压键输入     |
| `C / COM`     | `GND`   | 编码器公共端                          |
| `KEY_COM / T` | `GND`   | 按键公共端                           |


默认参数（`board_config.h`）：

- `BBCLAW_NAV_ENABLE=1`
- `BBCLAW_NAV_ENC_A_GPIO=6`
- `BBCLAW_NAV_ENC_B_GPIO=8`
- `BBCLAW_NAV_KEY_GPIO=1`
- `BBCLAW_NAV_PULL_UP=1`
- `BBCLAW_NAV_KEY_ACTIVE_LEVEL=0`

软件默认交互设计：

- 旋转：当前模式下执行上翻 / 下翻
- 短按：确认 / 选中当前滚动目标（当前实现为切换 `ME` / `AI` 焦点）
- 长按：切换导航模式（当前实现为 `scroll` / `history` 两种模式）

硬件说明：

- `A/B/KEY` 三路默认都使用内部上拉，外部可不再加上拉电阻。
- 若旋转方向与预期相反，互换 `A/B` 两根线即可。
- `GPIO1` 被保留给编码器按键，因此 `bbclaw` 板型默认关闭 `BBCLAW_SPEAKER_SW_GPIO`。
- 若切到 `ES8311` 模式，`GPIO6/8` 会与 `I2C SCL/SDA` 冲突，需要重新分配编码器引脚。

## 3. ES8311 兼容模式（可选）

对应配置宏：

- `BBCLAW_AUDIO_INPUT_SOURCE="es8311"`

### 2.1 ES8311+NS4150B 模块接线（含供电）


| 模块引脚  | 接到开发板         | 说明                                                                 |
| ----- | ------------- | ------------------------------------------------------------------ |
| `SDA` | `GPIO8`       | I2C 控制                                                             |
| `SCL` | `GPIO6`       | I2C 控制                                                             |
| `MCK` | `GPIO2`       | I2S 主时钟；若启用 RYG 状态灯，需改配 `BBCLAW_AUDIO_I2S_MCK_GPIO` 或 LED 红灯 GPIO |
| `BCK` | `GPIO16`      | I2S 位时钟                                                            |
| `WS`  | `GPIO15`      | I2S 字时钟                                                            |
| `DI`  | `GPIO18`      | I2S 输入（to ESP RX）                                                  |
| `DO`  | `GPIO17`      | I2S 输出（from ESP TX）                                                |
| `3V3` | `3V3`         | 数字域供电                                                              |
| `VCC` | `5V`（待模块铭牌确认） | 功放域常用 5V                                                           |
| `GND` | `GND`         | 共地                                                                 |


## 4. 供电与接线注意事项

- 所有模块必须共地（`GND` 共地）。
- 功放/马达优先走 `5V` 供电（按模块额定电压确认）。
- 麦克风数字模块（如 INMP441）使用 `3V3` 供电。
- 只有 `bbclaw` 板型默认启用电池采样，使用 `GPIO3 (ADC1_CH2)`，避免占用 `ADC2`。
- `GPIO45/GPIO46` 避免作为外设上拉输入使用。
- Wi-Fi/BLE 同时启用时，`ADC2` 不用于 ADC 采样功能。
- RYG 状态灯默认占用 `GPIO2/4/5`；若切到 `ES8311` 模式，默认 `MCK=GPIO2` 会与红灯冲突，需要二选一重映射。
- `BBCLAW_PA_EN_PROBE_GPIO1` 当前默认也是 `GPIO13`，但 `BBCLAW_PA_EN_PROBE_ON_BOOT=0` 默认关闭；若启用开机功放探测，需要避免和马达脚复用。
- `INMP441` 默认开启 `BBCLAW_AUDIO_RX_AUTO_CHANNEL_LOCK=1`，`L/R` 接 `GND` 或 `3V3` 都能自动选择非零声道。

## 5. 相关资料

- [硬件选型与资料缺口](./hardware_selection.md)
- [Adapter 对接说明](./bbclaw_adapter_integration.md)
- 模块图：
  - `INMP441.png`
  - `motor.png`
  - `ES8311+NS4150B.png`
