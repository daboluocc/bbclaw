# BBClaw 状态灯（PWM/WS2812）统一设计

> 状态：草案（Draft）
> 日期：2026-05-09
> 相关文档：[STATE_MACHINE.md](STATE_MACHINE.md)、[AGENT_STATE_MACHINE.md](AGENT_STATE_MACHINE.md)
> 相关代码：`firmware/src/bb_led.c`、`firmware/include/bb_led.h`、`firmware/src/bb_state.c`、`firmware/src/bb_power.c`

## 1. 背景与问题

BBClaw 板子有一颗 WS2812 可寻址 RGB（U6，DIN = GPIO5），既是启动跑马灯，也是运行期的"状态指示灯"。当前 `firmware/src/bb_led.c` 的实现有三个问题：

1. **状态维度只覆盖一个轴** — `bb_led_status_t` 只有 7 个值：
   `IDLE / RECORDING / PROCESSING / REPLY / NOTIFICATION / SUCCESS / ERROR`。
   这只映射了"PTT + ASR/TTS 流水线阶段"，没有反映 agent 九态、网络可达性、电池低电量。

2. **主动推送模型** — `bb_radio_app.c` 在 14 个点主动调 `bb_led_set_status()`
   喂状态。新增 agent 状态、新增页面、改网络检测流程都要记得同步喂一遍，
   很容易漏掉导致"灯和屏幕说的不一样"。

3. **语义重叠** — PROCESSING 和 ERROR 都是黄/红闪；REPLY 和 SUCCESS / NOTIFICATION
   在效果上只差 200ms 的 overlay；用户难以分辨。

## 2. 目标

- LED 反映设备**当前真实综合状态**，而不是"最近一次有人喂了什么"。
- 覆盖所有用户关心的维度：agent 九态、PTT 阶段、网络状态、电池低电。
- 每个效果有明确语义，用户凭颜色+节奏能快速判断"当前在做什么 / 哪里出了问题"。
- 接入成本低：上层业务不用再"记得去喂 LED"。

## 3. 架构

### 3.1 核心变化：主动推送 → 被动订阅 + 优先级合成

LED 任务不再暴露"设置这个状态"的入口，而是：

```
bb_led_task (30ms 周期):
  st   = bb_state_get()          ← agent / ptt / net / session / tts_in_flight...
  bp   = bb_power_get_state()    ← battery.percent / battery.low
  pulse = consume_overlay_if_any()
  effect = compose_effect(st, bp, pulse)   ← 按优先级表
  render(effect, now_ms)
  vTaskDelay(30ms)
```

`bb_state` 已经是全局状态的 Single Source of Truth（见
`design/STATE_MACHINE.md`），LED 直接消费快照即可。
`bb_radio_app.c` 里所有 `bb_led_set_status(...)` 调用都删除。

### 3.2 保留一个"短暂脉冲"入口

少数事件是"瞬时"的、状态本身没变化但值得提示一下：成功完成、出错、快速回复。
LED 模块保留一个 overlay 入口：

```c
typedef enum {
  BB_LED_PULSE_SUCCESS,    /* 绿色 200ms 实心闪一下 */
  BB_LED_PULSE_ERROR,      /* 红色 3 快闪，共 600ms */
  BB_LED_PULSE_CELEBRATE,  /* 粉色 1s 脉冲 */
  BB_LED_PULSE_NOTIFY,     /* 白色 400ms 闪一下 */
} bb_led_pulse_t;

esp_err_t bb_led_pulse(bb_led_pulse_t kind);
```

pulse 记录一个 `{ kind, start_ms, duration_ms }`，task 渲染时若 overlay 未到期
则以 overlay 为准；到期后回落到按快照合成的"基态"。

**事件触发点**（由 `bb_state` listener 处理，不是业务层散着调）：
- `BB_EVT_AGENT_ERROR` / `BB_EVT_ASR_ERROR` → `PULSE_ERROR`
- `BB_EVT_AGENT_TURN_END` 且 `turn_start_ms + 5s > now` → `PULSE_CELEBRATE`（对齐 HEART 状态）
- `BB_EVT_NOTIFICATION_RECEIVED`（display_pull 有新通知）→ `PULSE_NOTIFY`
- 普通 `BB_EVT_AGENT_TURN_END` → `PULSE_SUCCESS`

### 3.3 线程模型

- `bb_led_task` 任务，与现有实现相同。
- `bb_state_get()` 是 sequence lock，任意线程读安全；LED 任务无需持有 LVGL 锁。
- `bb_led_pulse()` 通过 `portENTER_CRITICAL` 改 overlay 记录，在任意上下文（含 ISR 外的 callback）调用安全。

## 4. 状态 → 效果映射表（优先级从高到低）

WS2812 用 (R, G, B) 8-bit，亮度在 `render()` 内按 `BBCLAW_STATUS_LED_BRIGHTNESS_PCT` 线性缩放。

| # | 触发条件 | 颜色 RGB | 动画 | 周期/时长 | 语义 |
|---|---|---|---|---|---|
| P0 | `pulse` overlay 活跃 | 按 pulse 定义 | 见 3.2 | 200–1000ms | 瞬时提示 |
| P1 | `agent == DIZZY` | (255, 0, 0) 红 | 慢闪 | 500ms on / 500ms off | Agent/ASR 出错，未恢复 |
| P2 | `net == OFFLINE` or `adapter_offline` | (180, 70, 0) 暗橙 | 呼吸 | 2000ms | 没网 / adapter 不可达 |
| P3 | `net == DEGRADED` | (200, 0, 255) 紫 | 呼吸 | 1000ms | 网络抖动 / 探测中 |
| P4 | `ptt ∈ {ARMED, STREAMING}` or `agent == LISTENING` | (0, 100, 255) 蓝 | 常亮（STREAMING 时可选亮度随 VAD 振幅） | — | 正在听用户说话 |
| P5 | `agent == ATTENTION` | (255, 200, 0) 黄 | 快闪 | 200ms on / 200ms off | 等待用户审批 tool_call |
| P6 | `agent == BUSY` or `ptt == RELEASED_WAIT` | (0, 255, 255) 青 | 脉动（sinusoidal） | 500ms | Agent 正在思考 / 流式输出 |
| P7 | `agent == SPEAKING`（`tts_in_flight`） | (0, 200, 160) 青绿 | 慢呼吸 | 2000ms | Agent 正在说（TTS 播放） |
| P8 | `agent == CELEBRATE` or `HEART` | (255, 80, 180) 粉 | 呼吸 | 1500ms | 完成 / 快速回复的持续态 |
| P9 | `battery.low && battery.percent >= 0` | (255, 120, 0) 橙 | 单闪 | 100ms on / 4900ms off | 电量低提示（不打扰） |
| P10 | `page == LOCKED` | (120, 0, 200) 紫 | 慢呼吸 | 3000ms | 等密语解锁 |
| P11 | 默认 (`agent ∈ {IDLE, SLEEP}` 且 net OK) | (0, 180, 90) 暖绿 | 微呼吸 | 4000ms | 待命 |

**优先级裁决**：从 P0 往下扫，第一个条件满足的就是当前效果。这保证：
- 出错、PTT 按下、网络异常这类"需要用户立即看到"的状态总是压住 agent 状态；
- 低电量提示只在空闲时出现，不会盖掉"正在听/正在说"的关键反馈。

### 4.1 Boot marquee（保留）

开机 3 色跑马灯保持现状：
- 由 `bb_led_init()` 在 `s_boot_anim_start_ms` 到 `BB_LED_BOOT_TOTAL_MS` 窗口内独占渲染。
- Boot 动画期间忽略 `bb_state` 和 pulse。
- Boot 结束后进入上面的合成逻辑。

### 4.2 关灯（省电/下电场景）

预留 `BBCLAW_STATUS_LED_ENABLE == 0` 编译开关；运行期无动态关灯。后续要加"深度休眠"可以通过 agent state 扩展（例如加 `BB_AGENT_STATE_DEEP_SLEEP`）再扩展此表。

## 5. 新 / 旧 API 对比

### 旧（被删）

```c
typedef enum {
  BB_LED_IDLE, BB_LED_RECORDING, BB_LED_PROCESSING,
  BB_LED_REPLY, BB_LED_NOTIFICATION, BB_LED_SUCCESS, BB_LED_ERROR,
} bb_led_status_t;

esp_err_t bb_led_set_status(bb_led_status_t status);
```

### 新

```c
typedef enum {
  BB_LED_PULSE_SUCCESS,
  BB_LED_PULSE_ERROR,
  BB_LED_PULSE_CELEBRATE,
  BB_LED_PULSE_NOTIFY,
} bb_led_pulse_t;

esp_err_t bb_led_init(void);                  /* 不变 */
esp_err_t bb_led_pulse(bb_led_pulse_t kind);  /* 新；替代所有 set_status 调用 */
```

`bb_radio_app.c` 的清理：
- 所有 `bb_led_set_status(BB_LED_IDLE/RECORDING/PROCESSING/REPLY)` 调用删除
  — 对应效果由 state 驱动。
- `show_status_error` 里改成 `bb_led_pulse(BB_LED_PULSE_ERROR)`，但基态还是由
  `agent == DIZZY` 或 `BB_EVT_*_ERROR` 事件把 agent 切到 DIZZY 来维持。
- `bb_led_set_status(BB_LED_SUCCESS)` → `bb_led_pulse(BB_LED_PULSE_SUCCESS)`。
- `bb_led_set_status(BB_LED_NOTIFICATION)` → `bb_led_pulse(BB_LED_PULSE_NOTIFY)`。

**最终结果**：`bb_radio_app.c` 里 `bb_led_*` 出现次数从 14 降到 ~2
（只保留 `bb_led_init()` + 可能的 boot pulse）。

## 6. 实施步骤

1. 写本设计文档（本文件）。
2. 重写 `bb_led.c/h`：
   - 新增 effect 表 + compose + pulse 队列。
   - 注册 `bb_state` listener，在 `AGENT_ERROR` / `ASR_ERROR` / `AGENT_TURN_END`
     事件里触发对应 pulse。
   - 保留 boot marquee 逻辑不动。
3. 清理 `bb_radio_app.c` 里的 `bb_led_set_status` 调用点。
4. `make build`，如果预览环境支持用 sim 先看一眼颜色。
5. 实机验证由用户完成：按优先级表逐条触发（拔网线、按 PTT、等 agent 回复、低电
   模拟）并对照上表。

## 7. 不做 / 未来扩展

- **多颗 LED**：当前 U6 只有一颗，不做动画级渐变（rainbow/chase）。
- **亮度自适应**：暂不读环境光；亮度仍由 `BBCLAW_STATUS_LED_BRIGHTNESS_PCT` 编译常量控制。
- **通过 LED 传递更精细信息**（session 数、tool_call 名字等）：应该上屏，不塞进灯。
- **用户可配色**：设置菜单里暂不开放 LED 主题；待产品稳定后再考虑。
