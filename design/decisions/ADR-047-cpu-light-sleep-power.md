# ADR-047：CPU/系统级低功耗 —— 自动 Light Sleep + DFS（参考威雪手表）

**状态**: Proposed
**作者**: BBClaw 团队
**日期**: 2026-07-11
**相关**: ADR-046（IMU 息屏管理，仅屏幕层）、ADR-040（Waveshare 拓展板）、
`boards/waveshare-amoled-206/`

---

## 问题

ADR-046 落地了**息屏状态机**（`ACTIVE → DIMMING → SLEEPING → WAKING`），空闲时
把 CO5300 AMOLED `DISPOFF` 关掉——这解决了**屏幕**这一路功耗。但真机实测续航仍远
达不到 ADR-046 预期（400mAh 待机 2–3 天），根因是：

> **息屏之后，SoC 依旧满血跑。** `CONFIG_PM_ENABLE is not set`，CPU 恒定
> 240MHz、无 tickless idle、无 light sleep，WiFi 常连不进 modem-sleep。屏幕黑了，
> 主控/射频的电流一分没省。

威雪（Waveshare）官方手表 Demo（同款 ESP32-S3-Touch-AMOLED-2.06）之所以能做到
数天待机，靠的不是关屏，而是 **CPU 动态调频（DFS）+ 自动 light sleep + WiFi
modem-sleep + AXP2101 电源域管理**这一整套系统级低功耗。本 ADR 把这套补齐，作为
ADR-046 的**系统层续集**。

**约束（产品级，不可破坏）**：BBClaw 是**云端语音助手**，息屏待机时仍需
**在线接收云端推送**（`bb_sleep_manager_on_message_arrived` → 消息唤醒）。因此
**不能用 deep sleep**（deep sleep 断连、丢 RAM、整机重启，push 收不到）。方向必须
是 **light sleep（保连接、保 RAM、微秒级恢复）**。

---

## 方案

### 分层：屏幕层（已有 ADR-046） × 系统层（本 ADR）

| 层 | 机制 | 状态 |
|----|------|------|
| 屏幕 | 息屏状态机 → `DISPOFF` + 亮度渐变 | ✅ ADR-046 已落地 |
| **CPU** | **DFS 240↔80MHz + tickless idle** | 🆕 本 ADR |
| **CPU** | **自动 light sleep（空闲即睡，中断/beacon 唤醒）** | 🆕 本 ADR |
| **射频** | **WiFi modem-sleep（DTIM beacon 维持关联）** | 🆕 本 ADR |
| PMIC | AXP2101 充电参数（已有）+ 睡眠断非必要轨（phase 2） | ⏳ 部分 |

两层**解耦但联动**：息屏状态机是「用户意图空闲」的唯一真相源，系统层**订阅它的
状态**决定何时允许 SoC 深睡。

### 1. esp_pm 自动 light sleep + DFS（本期核心）

**sdkconfig（板级 overlay，仅手表板生效，不碰 bbclaw 量产 PCB / OTA）**：

```
CONFIG_PM_ENABLE=y
CONFIG_FREERTOS_USE_TICKLESS_IDLE=y
CONFIG_PM_DFS_INIT_AUTO=y          # 运行时仍显式 esp_pm_configure 覆盖
# CPU 不在 light sleep 里断电（保守：与 Octal PSRAM + QSPI LCD 共存更稳）
# CONFIG_PM_POWER_DOWN_CPU_IN_LIGHT_SLEEP is not set
```

> 放在 `boards/waveshare-amoled-206/sdkconfig.board`。`bbclaw` 量产板不含这些行，
> `release.yml` 用 `sdkconfig.bbclaw.latest` 构建，**本改动 0 影响 OTA**。等手表板
> 真机功耗/稳定性验证充分后，再评估是否推广到量产板。

**运行时（`bb_pm_init`）**：

```c
esp_pm_config_t cfg = {
    .max_freq_mhz = 240,   // 交互/音频时全速
    .min_freq_mhz = 80,    // 空闲降频（80 与 APB/WiFi 兼容，保守起步）
    .light_sleep_enable = true,
};
esp_pm_configure(&cfg);
```

`light_sleep_enable=true` 的语义：**只要没有任何 PM lock 且系统 idle，就自动进
light sleep**，任意中断（GPIO/timer/WiFi RX）在数十微秒内恢复，对上层完全透明。

### 2. PM lock —— 用息屏状态门控深睡

自动 light sleep 一旦全局打开，交互中若也随便睡会拖慢 UI/掉音频。用 lock 控制：

- 持 `ESP_PM_NO_LIGHT_SLEEP` lock ⇒ 禁 light sleep（但 DFS 仍可降频）。
- 释放该 lock ⇒ 允许 light sleep。

**联动规则**（`bb_power_mgmt_tick` 每轮读息屏状态）：

| 息屏状态 | NO_LIGHT_SLEEP lock | 效果 |
|----------|---------------------|------|
| `ACTIVE` / `DIMMING` / `WAKING` | **持有** | 屏亮/交互，禁深睡，UI 跟手 |
| `SLEEPING` | **释放** | 屏灭、真空闲，SoC 在 beacon 间隙 light-sleep |

> DFS 与音频：ESP-IDF 的 I2S/I2C/QSPI 驱动在 PM 打开时会自行申请 `APB_FREQ_MAX`
> lock，PTT 录音 / TTS 播放期间自动锁频锁睡，无需上层介入。故本 ADR 只管
> **NO_LIGHT_SLEEP** 这把「交互锁」。

**message-wake 不变**：SLEEPING 时 WiFi 关联仍在（modem-sleep 靠 DTIM beacon 维持），
云端 push 到达 → WiFi RX 中断唤醒 SoC → `bb_adapter_client` 收包 →
`bb_power_mgmt_on_message_arrived` → 息屏转 `WAKING/ACTIVE` → `bb_pm` 重新持锁。整条
链路与现状一致，只是空闲间隙 SoC 真正睡了。

### 3. IMU 轮询节流修复（本期，light sleep 生效前提）

`qmi8658_sample_task` 的 `delay` 在 `while` 循环**之前**只算一次（`qmi8658.c` L79），
`bb_imu_enable_low_power()` 运行时把采样率改到 16Hz **对已在跑的任务无效**——任务
仍每 10ms 唤醒 CPU 一次。这样即便开了 light sleep，CPU 也被 IMU 每 10ms 拽醒，深睡
残留时间趋近 0，省电落空。

**修复**：把 `delay` 计算移进循环体，按当前 `sample_rate_hz` 每轮重取。SLEEPING 降到
16Hz 后 CPU 唤醒周期 10ms → 62ms，light sleep 才有可睡的窗口。

### 4. Phase 2（后续，需原理图确认，不在本期）

- **IMU 硬件中断唤醒**：QMI8658 INT1 wake-on-motion（板载已接 GPID），息屏后**停轮询
  任务**，靠运动中断把 SoC 从 light sleep 拉起——CPU 可连续深睡数秒而非 62ms 一醒，
  省电量级提升。需确认 INT1 GPIO 走线。
- **AXP2101 睡眠断轨**：SLEEPING 时关掉不供 MIC/触摸唤醒的电源域（如 DLDO/BLDO 上挂
  的外设），唤醒再上电。**需原理图逐轨确认**——误关 ALDO1（MIC 轨）会收音失声，误关
  触摸轨会丢唤醒源，故本期不做，先只保留已有充电参数配置。

---

## 实施路线图

### Phase 1（本 ADR，本期落地）
- [x] `bb_pm` 模块：`esp_pm_configure` + NO_LIGHT_SLEEP lock，门控化
- [x] 接入 `bb_power_mgmt`（init + tick 按息屏状态持/放锁）
- [x] `sdkconfig.board` 打开 PM_ENABLE + TICKLESS_IDLE（仅手表板）
- [x] 修 `qmi8658` 采样率运行时切换 bug
- [ ] 真机烟测：boot、音频、USB、状态转换、message-wake 无回归
- [ ] 真机功耗实测（万用表）：SLEEPING 平均电流 vs 改前

### Phase 2（后续 ADR）
- [ ] QMI8658 INT1 wake-on-motion，息屏停轮询
- [ ] AXP2101 睡眠电源域管理（需原理图）
- [ ] 评估推广到 bbclaw 量产板 sdkconfig

---

## 风险 & 缓解

| 风险 | 缓解 |
|------|------|
| light sleep 打断 I2S 音频出杂音 | I2S 驱动 PM 下自动持 APB 锁；交互态 NO_LIGHT_SLEEP 锁；真机听感验证 |
| QSPI LCD / Octal PSRAM 与 light sleep 兼容性 | 本期**不**断 CPU 电（`POWER_DOWN_CPU` 关）；仅时钟门控，最保守档起步 |
| 开发期 TinyUSB CDC 被 light sleep 掐断 | 仅 SLEEPING 才深睡，调试时屏常亮/短超时；量产无 USB，可接受 |
| 全局 PM 影响时序敏感外设 | 板级 overlay 只作用手表板，量产 PCB/OTA 零影响；出问题一键关 `BBCLAW_PM_LIGHT_SLEEP_ENABLE` |
| 省电不达预期（IMU 62ms 仍频繁醒） | 已知，Phase 2 用 IMU 硬件中断根治；本期先拿 DFS + 音频/射频空闲的确定收益 |

---

## 预期收益（本期，DFS + auto light sleep，IMU 仍 62ms 轮询）

- SLEEPING 平均电流：CPU 从 240MHz 满载降到 80MHz + beacon 间隙 light-sleep，
  叠加 WiFi modem-sleep，预计**待机电流降一个量级**（真机万用表核实）。
- 功能**零损失**：抬手唤醒、消息唤醒、音频、OTA 全保留。
- Phase 2（IMU 中断 + AXP 断轨）后有望逼近 ADR-046 的数天待机目标。

---

## bbclaw 生产板落地（LCD 无 AMOLED，无 IMU）

手表是本 ADR 的首落点，bbclaw 生产板同一套思路但硬件不同，差异如下：

| 维度 | 手表(WS AMOLED-206) | bbclaw 生产板 |
|------|--------------------|--------------|
| 屏 | QSPI CO5300 AMOLED（自发光） | SPI ST7789 TFT-LCD |
| 息屏机制 | DISPOFF(0x28)+SLPIN(0x10) 熄像素 | **关背光 GPIO14**（LCD 功耗大头）+ DISPOFF(0x28) |
| 唤醒源 | IMU 抬手 + 触摸 + 消息 | **无 IMU/无触摸** → 导航轮键(GPIO1/6/8) + 消息 |
| 电量 | AXP2101 电量计 | ADC 分压(GPIO3) |

**实现**：
- 新增 `bb_display_control_st7789.c`：`set_brightness_raw`(背光 GPIO on/off)+
  `set_panel_on`(DISPOFF/DISPON + 背光)。与 `bb_display_control_co5300.c` 按显示总线
  互斥（`#if BBCLAW_DISPLAY_BUS_QSPI` vs `#if !...`）避免重复符号。
- bbclaw `board_config.h` 打开 `DISPLAY_BRIGHTNESS_CONTROL` + `SLEEP_MANAGER_ENABLE`
  （`IMU_WAKE=0`，`MESSAGE_WAKE=1`）+ `PM_LIGHT_SLEEP_ENABLE`。
- 导航键已接 `bb_power_mgmt_on_user_activity`（自动唤醒），无需改输入层。

**背光暂为 on/off**（非 PWM）：DIMMING 态背光保持亮，SLEEPING 才关。真·调光需给
BL 脚接 LEDC PWM，列为增强。

**OTA 边界（重要）**：息屏（sleep manager + 背光控制）由 `board_config.h` 宏开启，会
随 bbclaw OTA 出厂。但 **CPU light-sleep 的 `CONFIG_PM_ENABLE` 只放本地
`sdkconfig.pm` overlay，不进 `sdkconfig.bbclaw.latest`（OTA 生产配置）** —— light-sleep
影响面大（音频/USB/时序），真机功耗+回归验证充分前不推全量设备。`bb_pm` 在未开
`CONFIG_PM_ENABLE` 的 OTA 构建里自动降级 no-op。

## 审批

- **Author**: BBClaw 团队
- **Reviewer**:
- **Approved**:
