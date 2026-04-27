# ADR-008: Chat 作为待机首页 + 90s 空闲退出（Phase 4.8.x）

- **编号**: ADR-008
- **标题**: 待机屏作为首页；任意活动召唤 Agent Chat overlay；90s 无活动自动退出
- **日期**: 2026-04-27
- **状态**: 已接受
- **关联**: ADR-007（Settings overlay）；Phase 4.5（语音桥接到 Agent Bus）；Phase 4.6（待机/IDLE 主题）

## 背景

### Phase 4.2 — 长按 BACK 召唤

Agent Chat 最初是一个**显式召唤**的 overlay：

- 主屏显示 legacy `bb_lvgl_display` 的聊天历史 / 雷达 UI
- 用户**长按 BACK** 才进入 Agent Chat overlay
- 退出也是 BACK
- PTT 按下时如果 chat 不在前台，语音转写"看不见"（因为 Phase 4.5 的语音桥接路径只在 chat active 时才把 ASR 文本灌入 Agent Bus）

这意味着用户**必须先记得开 chat 再说话**，否则 PTT 字面上是个"开了麦但没有人接听"的死动作。

### Phase 4.8 — 启动即 chat（已废弃）

commit `1ebc765` 翻到了另一极端：**开机直接进 Agent Chat，永不退出**。

理由当时是"反正所有事都要走 chat，那就别让用户管开/关"。

真机两小时后用户反馈：

> "我想要一个待机屏，不是 chat 一直挂在那。Chat 一直亮着我不知道它'活着'还是
> '在等我'，分不清。一个静态的 standby 我能接受'设备在那但没在做事'。"

也就是说，**chat overlay 的存在本身**承载了"AI 正在听我"的语义；让它常驻反而稀释了这个信号。

### Phase 4.8.x — 待机优先 + 活动召唤

最终设计（commit `57da618`）：

- 开机落到 **legacy standby 屏**（保留 Phase 4.6 的待机视觉）
- **任意用户活动**（PTT 按下 OR 任何 nav 按键）→ 自动召唤 Agent Chat overlay
- **90 秒无活动** → 自动退出 chat overlay 回到待机
- 任何活动都重置空闲计时器
- 与 Settings overlay（ADR-007）互斥：Settings 开着时不召唤 chat

## 决策

### 1. 状态机

```
                       任意活动
                  ┌─────────────────┐
                  ▼                  │
   ┌─────────┐  90s 无活动     ┌───────────┐
   │ standby │ ←────────────── │ chat-active │
   └─────────┘                  └───────────┘
                                      │
                                  Settings 互斥
                                      ▼
                                 ┌──────────┐
                                 │ settings │
                                 └──────────┘
```

### 2. 召唤触发器（auto-enter）

`bb_radio_app` 的 stream task 在以下事件来时**先检查应否召唤**，再做本职处理：

- `bb_audio_stream_event_ptt_press`
- 任何 `bb_nav_event_t`（UP/DOWN/LEFT/RIGHT/OK/BACK）

召唤前置条件：

```c
if (!bb_ui_settings_is_active() &&
    !bb_ui_agent_chat_is_active()) {
    bb_ui_agent_chat_enter_locked();
}
```

PTT 路径与 nav 路径都用同一个谓词。注意：**Settings overlay 内的 PTT 也不会召唤 chat**，因为用户此时显然在做配置，不该被"踹"出去。

### 3. 退出触发器（idle exit）

stream task 维护：

```c
static int64_t s_last_activity_ms = 0;
#define BB_CHAT_IDLE_TIMEOUT_MS  (90 * 1000)
```

每个事件循环 tick 检查：

```c
bool should_idle_exit =
    bb_ui_agent_chat_is_active() &&
    !bb_ui_settings_is_active() &&
    !bb_agent_session_in_flight() &&    /* 关键：不打断进行中的回合 */
    (now_ms - s_last_activity_ms) > BB_CHAT_IDLE_TIMEOUT_MS;

if (should_idle_exit) {
    bb_ui_agent_chat_exit_locked();
}
```

**`!bb_agent_session_in_flight()` 这个条件至关重要**：

- 如果 agent_task 正在跑、或 TTS 在播、或网络回合未结束，就**不**退出
- 否则用户问个问题 → 等 95 秒回复（长答案 + 慢网络）→ chat 突然消失，用户体验灾难
- 进行中的回合本身**也算活动**，自然延长保留时间

### 4. 活动定义（reset clock）

什么事件会更新 `s_last_activity_ms`：

| 事件 | 算活动？ | 备注 |
|---|---|---|
| PTT 按下 / 释放 | 是 | 用户显式说话 |
| nav 按键（任意方向） | 是 | 用户显式翻页 |
| ASR 进入 LISTENING | 是 | 已经隐含在 PTT 触发里 |
| Agent 回流 EvText delta | 否 | AI 单方面输出不算用户活动 |
| TTS 播放中 | 否（但 in_flight 阻断 idle exit） | 由 in_flight 谓词 cover |
| EvTurnEnd | 否 | 标记 in_flight 解除，从此 90s 倒计时重新生效 |

### 5. 为什么这样设计

> **Standby 把 legacy 聊天历史视图保留下来作为"低能耗、下班后"的静态外观**，
> 同时所有交互式 PTT **依旧通过统一的 Agent Bus 路径**（Phase 4.5 voice
> bridge 已经在 chat-active 时门控住了）。

也就是说：

- 待机屏 = 设备外观 / 心情灯 / 历史回顾
- chat overlay = "AI 正在与我对话"的活跃指示
- Settings overlay = "我在配置设备"的活跃指示

三种状态在视觉上**清晰区分**，用户瞄一眼就知道当前模式。

而且这种设计避免了 Phase 4.8 初版的两个问题：

1. **"chat 是否活着"模糊** —— 现在 chat 只在有交互前后的 90s 窗口可见，存在即代表"刚发生过对话"
2. **PTT 死动作** —— PTT 按下会主动召唤 chat，不再要求用户先开 chat

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 永远显示 chat（Phase 4.8 初版） | 用户反馈"分不清待机/活跃" |
| 长按 BACK 召唤（Phase 4.2 设计） | PTT 是死动作；用户得记得"先开 chat 再说话" |
| 召唤后永不退出（要求显式 BACK） | 用户走开后 chat 一直亮着，徒增 OLED 老化 + 视觉噪声 |
| 30s / 60s 空闲退出 | 太短，正常思考下一句话就被踹回待机；90s 经测试是"足够慢回话也能保持"的甜点 |
| NVS 可配 timeout | 当前没必要复杂化；ADR-007 提到未来加 |
| PTT 不召唤、只 nav 召唤 | 违反"PTT 必须看得见反馈"的核心 UX，PTT 才是设备主用法 |

## 后果

### 正面

- 待机屏保留了 Phase 4.6 的视觉资产（idle/heart 主题不被 chat overlay 遮挡）
- PTT 即"开始对话"，0 步入门
- 90s 空闲是大多数用户的"自然遗忘节点"，过了这个窗口设备就回归静默
- in_flight 守卫确保长回合不会被截断
- 与 Settings overlay 自然互斥，不需要额外仲裁

### 负面 / 风险

- 90s 是硬编码 magic number。慢用户 / 多语言用户 / 老人可能嫌短
- nav 按键触发召唤可能在用户**只是想关 Settings BACK 一下**时误召唤 chat
  → 实际不会，因为 BACK 在 Settings 内被 Settings 消费，不会冒泡到 stream task 的"无 overlay 时 nav"分支
- 锁屏 / 物理锁状态没考虑 —— 如果设备未来加锁屏，召唤逻辑要先过 lock 谓词
- 待机屏渲染和 chat overlay 渲染都要走 LVGL，频繁来回切可能引入 redraw 抖动；
  目前未观察到，但 90s 频率下问题不大

### 未来工作

- 把 90s 做成 NVS 可配项（ADR-007 已留接口位）
- 锁屏支持：
  - 锁定状态下 PTT 不触发任何召唤（直接静默丢弃，或 LED 闪一下提示"锁着呢"）
  - nav 按键也只能解锁，不能召唤 chat
  - 配合 ADR-009 的 SLEEP 状态，锁屏可视为"长睡眠"
- 在 chat overlay 即将自动退出前 ~5s，给个轻微视觉脉冲（呼吸光 / icon 弱闪），让用户有"快关了"的预感，避免突兀
- "Recent activity" 入口：从待机屏上滑能看到最近一次 chat 的最后几条消息（无需召唤完整 chat overlay），适合"我刚才问了啥来着"场景
