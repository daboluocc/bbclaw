# M5Stack M5StickS3 适配设计

状态: bring-up 完成（显示/音频/WiFi/cloud_saas 真机跑通）→ 产品化设计（充电/电池双模式 + 精简顶栏）
日期: 2026-07-24
引脚真相来源: **M5Stack 官方 K150 StickS3 datasheet / 原理图**（已逐项验证，100% 吻合）。

## 1. 硬件概况

- **SoC/模组**: ESP32-S3-PICO-1-N8R8 — 8MB flash + **8MB Octal PSRAM**
  （⚠ 必须 `CONFIG_SPIRAM_MODE_OCT`，否则 `wrong PSRAM line mode` boot loop）
- **显示**: 1.14" SPI **ST7789P3 135×240 窄竖屏**（无触摸）；背光 GPIO38
- **音频**: **ES8311 收放一体**（DAC 出喇叭经 AW8737 功放；MEMS mic 走 ES8311 自带 ADC，无 ES7210）
- **PMIC**: **M5PM1**（自研，I2C 0x6E，非 AXP）——屏/mic/喇叭的 **L3B 供电轨**挂它 GPIO2，
  功放使能挂 GPIO3，电量/充电/电源键都在它后面
- **IMU**: BMI270（I2C 0x68），INT 经 M5PM1
- **红外**: TX G46 / RX G42（本适配不使用）
- **按键**: KEY1=G11（前）、KEY2=G12（侧）；另有 M5PM1 电源键（长按=下载、双击=关机、单击=开机）
- **电池**: 250mAh；**满/空电压 4200/3300mV**

### 引脚表

| 功能 | GPIO |
|------|------|
| LCD MOSI/SCLK/DC/CS/RST/BL | 39 / 40 / 45 / 41 / 21 / 38 |
| I2C SDA/SCL（ES8311 0x18 / BMI270 0x68 / M5PM1 0x6E 共享 port0） | 47 / 48 |
| I2S MCLK/BCLK/WS/DOUT/DIN | 18 / 17 / 15 / 14 / 16 |
| 按键 KEY1(PTT) / KEY2(nav) | 11 / 12 |

### 引导关键（勿回退）
- OCTAL PSRAM；8MB DIO；标准 `partitions_ota.csv`（无摄像头，2.5MB 槽够）。
- **M5PM1 最小上电必须先于探 ES8311**：开 L3B（GPIO2=reg0x11 bit2 拉高）→ codec 才有电，
  否则 `detect_codec_address` I2C NAK → abort → boot loop（真机踩过）。见 `bb_audio.c m5pm1_minimal_init`。

## 2. 交互设计（无触摸 · 2 键 · 体感唤醒）

M5StickS3 无触摸屏，靠 2 个可编程键 + IMU：

- **KEY1（G11）= PTT**：按住说话（与实体对讲同链路）。
- **KEY2（G12）= 导航键**：**短按 OK / 长按 BACK**（单键走 Flipper 路径，其余方向键 -1）。
- **BMI270 = 拿起唤醒**：`|a|` 偏离 1g（线加速度）→ 唤醒亮屏。**不做体感倾斜导航**
  （实测难调、非行业主流；IMU 的正确用途是唤醒/电源管理/粗手势）。
- 上下滚动列表：靠 KEY2 的 OK/BACK + 未来可选 BMI270 硬件双击（暂不做）。

## 3. 窄屏 UI 适配（135×240）

统一模式：页面几何 `#if BB_UI_PORTRAIT` 内再套 **`#if BB_DISP_W <= 160`** 窄屏分支
（把手表 410px 的点阵/halo 尺寸缩到 ~一半），本地 SDL 模拟器逐页渲染验证。

已适配：开机动画、待机时钟、聊天页（删屏上大 PTT 圆钮 + 顶栏就绪图标）、locked、
配网/netconn、ota 确认、prompt_select（倒计时条 356→119px + 选项限高截断"…"）、
录音页（halo 缩小 + 文字 2 行不重叠）。

## 4. 精简顶栏 ★

135px 宽度极紧张，顶栏**只保留三样：时间 · WiFi · 电量**。

- **去掉**：`s_img_mode`（HOME/CLOUD 指示）、"BBClaw" 字样、就绪状态图标（已删）、driver 名等。
- **布局（窄屏）**：左 WiFi 条 → 中/右 时间 → 最右 电量图标 + "NN%"。
- 实现：在顶栏构建处按 `BB_DISP_W <= 160` 隐藏/不创建非必要元素，只排 WiFi/clock/battery。

## 5. 电源双模式：充电 vs 电池 ★（核心）

**充电与电池是两套设计**——充电时不缺电、要当桌面时钟；电池时要激进省电。依 `charging`
（VBUS>4.2V，M5PM1 reg0x24/25）动态切换：

### 电池模式（拔 USB）
- 激进省电：变暗 **15s** → 熄屏（DISPOFF）**30s**（preset 默认，可设置里改）。
- 熄屏后：**拿起/晃动（BMI270）/ 按键 / 云消息**唤醒。
- 目标：保住 250mAh 续航。

### 充电模式（插 USB，VBUS 到位）
- 当**桌面充电时钟**：待机不熄屏，显示 **大时间 + 电量% + ⚡ 充电**（低亮度常显，防烧屏可轻微呼吸/挪位）。
- **息屏时长拉长 / 不息屏**（有外部供电，不怕费电）。
- 拔掉 USB → 立即回到电池模式的省电策略。

### 实现要点
- `bb_sleep_manager` 增加「充电时走 ambient-standby（时钟常显）、电池时走 DISPOFF」的**运行时**切换
  （现有 `BBCLAW_SLEEP_MANAGER_AMBIENT_STANDBY` 是编译期；改为按 `bbclaw_power_is_charging()` 动态）。
- 充电时钟视图复用待机页（`bb_page_standby`：大点阵时钟 + 电量），充电时额外显示 ⚡。
- 充电态由 UI 任务的电量 poll（时钟定时器，每 5s）驱动切换。

## 6. 电量 / 充电（M5PM1）

- **电量**：VBAT reg0x22(L)/0x23(H) 16-bit LE = 毫伏 → `battery_percent_from_mv`（4200/3300mV）。
- **充电/外部供电**：VBUS reg0x24/0x25 > 4200mV（比 PWR_SRC 位编码稳）。
- **轮询**：在 **UI 任务的时钟定时器**（每 5s、仅状态变化时）调 `bb_power_refresh` + 轻量
  `apply_battery_widget`（**不可**在后台小栈任务里调 `refresh_ui`，会栈溢出 boot loop——真机踩过）。
- 见 `bb_power.c` M5PM1 后端 + `bb_lvgl_display.c clock_timer_cb`。

## 7. 状态与 TODO

**已完成**：M5PM1 上电修 boot loop、显示/音频/WiFi/cloud_saas、窄屏 UI 全页、侧键 OK/BACK、
BMI270 拿起唤醒、电量/充电读取 + 待机刷新、息屏 preset 默认 30s。

**本设计新增待做**：
1. 精简顶栏（只留 时间/WiFi/电量）。
2. 充电/电池双模式（充电=桌面时钟常显 + 长息屏；电池=激进省电）。

**已知/护栏**：
- 后台任务调 `refresh_ui` 栈溢出 → 待机电量刷新必须走 UI 任务时钟定时器 + `apply_battery_widget`。
- CI `release.yml` 只构建 bbclaw 生产配置，M5 目前仅本地烧录（要 OTA 需扩 release + 云端加 platform）。
- 烧录：运行态即稳定 USB-JTAG，`esptool --before default_reset` 免长按可烧；MAC 14:c1:9f 验身。
