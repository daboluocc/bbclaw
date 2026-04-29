# ADR-012: 固定三页菜单（Standby / Chat / Settings）取代 overlay 召唤模型

- **编号**: ADR-012
- **标题**: 把 overlay 召唤模型重构为三个固定页面 + 显式四态状态机
- **日期**: 2026-04-30
- **状态**: 已接受
- **关联**:
  - **Supersedes** [ADR-007](ADR-007-standalone-settings-overlay.md)（独立 Settings overlay）
  - **Supersedes** [ADR-008](ADR-008-chat-as-standby-and-idle-exit.md)（chat 作为待机 + 90s idle exit）
  - 保留 [ADR-006](ADR-006-flipper-full-nav-events.md)（按键事件源）、[ADR-009](ADR-009-agent-state-machine.md)（agent 9 态，独立于本 ADR 的页面态）
  - 来源：[issue #15](https://github.com/daboluocc/bbclaw/issues/15)

## 背景

ADR-007 把 Settings 抽成独立 overlay，ADR-008 把 chat overlay 设计成"待机屏 + 任意活动召唤 + 90s idle 退出"。这两个 ADR 落地后，真机两个反馈：

1. **三层叠加心智负担**：设备同时存在 base display（standby/locked/active）+ chat overlay + settings overlay 三层，互斥规则散落在 [bb_radio_app.c](../../firmware/src/bb_radio_app.c) 的 stream task 里，每加一个 overlay 就要改一处仲裁逻辑。
2. **PTT 行为不可预测**：PTT 既能"召唤 chat"又能"在 chat 内录音"又能"在 LOCKED 里走密语验证"，三种语义共用一个按键，用户分不清当前按下去会发生什么。
3. **cloud_saas 与 local_home 体验不一致**：local_home 没有密语解锁，但开机后还是会经过 LOCKED → 自动 UNLOCKED 的过场，本质上是死代码。

ADR-007/008 的 overlay 模型在 v0.3 时是合理的（功能少、互斥简单），但到 v0.4 的多 driver / 多模式下已经溢出。Issue #15 要求换成"页面"模型：每个时刻**有且仅有一个**全屏页面在前台，状态机显式枚举。

## 决策

### 1. 四态显式页面状态机

```c
typedef enum {
  BBCLAW_STATE_LOCKED   = 0,  /* cloud_saas only: 待密语解锁 */
  BBCLAW_STATE_STANDBY  = 1,  /* 待机首页 */
  BBCLAW_STATE_CHAT     = 2,  /* Agent Chat 页面 */
  BBCLAW_STATE_SETTINGS = 3,  /* Settings 页面 */
} bb_radio_app_state_t;
```

**初始态选择**（开机 / boot）：

| Transport 模式 | 初始态 | 理由 |
|---|---|---|
| `cloud_saas` | `LOCKED` | 保留密语解锁能力（沿用现有 `passphrase_unlock_enabled()` 谓词） |
| `local_home` | `STANDBY` | 本地模式无密语机制，开机直达待机（issue #15 选项 3C） |

> 这与现有 `passphrase_unlock_enabled()` 的实现一致——它本来就只在 `bb_transport_is_cloud_saas()` 时返回 true。本 ADR 把这个隐含行为提升为显式状态。

### 2. 状态转移图

```
                 ┌─────────────┐
   (cloud)  ───→ │   LOCKED    │
                 └─────────────┘
                       │  PTT-talk + 密语验证通过
                       ▼
   (local)  ───→ ┌─────────────┐ ←──── BACK ──── ┌──────────┐
                 │   STANDBY   │                  │   CHAT   │
                 └─────────────┘ ──── BACK ────→ └──────────┘
                       │  ▲                            ▲
                       OK │                            │ (BACK 在 STANDBY 时直跳 CHAT，
                       │  BACK                          见按键表)
                       ▼  │
                 ┌─────────────┐
                 │  SETTINGS   │
                 └─────────────┘
```

**关键不变量**：

- 任何时刻**只有一个**页面在前台（全屏），不再叠加 overlay。
- `LOCKED` 态在 `local_home` 模式下永不进入。
- 自动 idle-exit（ADR-008 的 90s 计时器）**移除**。CHAT/SETTINGS 留在前台直到用户 BACK 退出。这是行为上的破坏性变更，理由见 §7。

### 3. 按键路由表（Flipper 6-button，依 ADR-006）

| 页面 | UP | DOWN | LEFT | RIGHT | OK | BACK | PTT |
|---|---|---|---|---|---|---|---|
| **LOCKED** (cloud) | ignore | ignore | ignore | ignore | ignore | ignore | **按住录音 → 松开走密语验证**（复用现有 `BB_STATUS_VERIFY` 流程，issue #15 选项 2A） |
| **STANDBY** | ignore | ignore | ignore | ignore | → SETTINGS | → CHAT | ignore |
| **CHAT** | 滚动历史 | 滚动历史 | 上一 driver | 下一 driver | ignore | → STANDBY | 按住录音 → 松开发送 |
| **SETTINGS** | 上移行 | 下移行 | 值预览(-) | 值预览(+) | 提交并落 NVS | → STANDBY (放弃未提交预览) | ignore |

**LEFT/RIGHT 在 SETTINGS 仅做内存预览，OK 才 commit 落 NVS** —— 这条是 ADR-007 §3 的硬规则，由 Phase 4.7.1 的 task_wdt 重启事故得出，本 ADR **保留不变**。

### 4. LOCKED 屏 UI（cloud_saas）

```
┌──────────────────────┐
│        🔒            │
│                      │
│  请按住说话键，       │
│   说出密语           │
│                      │
└──────────────────────┘
```

PTT 按下 → 状态条切到 `BB_STATUS_VERIFY_TX`（沿用 [bb_radio_app.c:1671](../../firmware/src/bb_radio_app.c) 已有路径），松开 → `BB_STATUS_VERIFY` 处理中 → 成功转 STANDBY，失败 `BB_STATUS_VERIFY_ERR` 后回 LOCKED。**密语验证逻辑零改动**，只把"触发入口"从隐式（任何时机）收敛到"LOCKED 态的 PTT"。

### 5. STANDBY 屏 UI

```
local_home 模式                 cloud_saas 模式
┌──────────────────────┐       ┌──────────────────────┐
│  设备状态            │       │  设备状态 + 配对码   │
│                      │       │                      │
│                      │       │                      │
│  [OK]设置  [BACK]聊天│       │  [OK]设置  [BACK]聊天│
└──────────────────────┘       └──────────────────────┘
```

底部恒显两条按键提示。中间内容沿用现有 standby 渲染（电量/网络/配对码/buddy 表情），不在本 ADR 范围。

### 6. Settings 简化（移除 Session 行）

当前 [bb_ui_settings.c](../../firmware/src/bb_ui_settings.c) 的 `settings_row_t` 是 5 行：

```c
ROW_AGENT, ROW_SESSION, ROW_THEME, ROW_TTS, ROW_BACK
```

新版 4 行：

```c
ROW_AGENT, ROW_THEME, ROW_TTS, ROW_BACK
```

Session 行（per-driver session list 异步拉取）整段删除。理由：

- Session 列表只对 claude-code 有意义，其它 driver 都是空，行为不一致。
- 切换 session 是低频操作，不值得占据 settings 黄金位。
- 真要切 session 的用户走 adapter HTTP API（`/api/agent/sessions`）更合适。

被删除的代码：
- `ROW_SESSION` enum 值
- `s_st.session_*` 字段及 fetch/render 逻辑
- 相关 NVS key（如有）

### 7. 移除 idle-exit（破坏性变更 vs ADR-008）

ADR-008 的 90s idle-exit 在新模型下移除，理由：

- 旧模型下 chat 是 overlay，"自动退出"等价于"撤掉浮层、露出待机"，视觉上自然。
- 新模型下 CHAT 是页面，"自动退出"等价于"用户没动、设备替他按了 BACK"，违反"页面切换由用户显式触发"的原则。
- 真机反馈：90s 太短，用户思考下一句就被踹回 STANDBY。

代价：CHAT 页面停留时间不限。OLED 老化 / 功耗成本由用户自己 BACK 控制。后续若有数据支持，可作为 Settings 一行加回（"chat idle timeout: off / 90s / 5min"）。

### 8. CHAT 中途按 BACK 的语义

CHAT 处于 BUSY / SPEAKING（agent 正在生成或 TTS 在播）时，用户按 BACK 的行为：

**取消正在进行的回合并立即返回 STANDBY。**

实现要点：
- 等同于现有的"用户 cancel"路径（[bb_ui_agent_chat.c:1367](../../firmware/src/bb_ui_agent_chat.c)），先调 `bb_agent_cancel_in_flight()` / TTS task cancel，再切 `s_app_state = STANDBY`
- 已经发送但未消费的 stream 帧丢弃
- 已上传的 ASR 音频不退（云端配额已扣，无法回滚）

**为什么选这个方案**：

| 备选 | 不选的理由 |
|---|---|
| 忽略 BACK，等回合结束 | 违反"按键行为可预测"原则；用户感觉"按了不理我" |
| 后台跑完、前台回 STANDBY | TTS 在 STANDBY 屏继续播放语义混乱；需要 agent task 与页面解耦的额外代码 |

代价：可能浪费一次正在路上的 cloud API 调用。但"用户随时能退出"的可预测性比节省一次调用重要。

### 9. PTT 路由收敛

| 状态 | PTT 行为 |
|---|---|
| LOCKED | 密语验证录音 |
| STANDBY | 忽略（不再"召唤 chat"） |
| CHAT | 录音 / 发送 |
| SETTINGS | 忽略 |

旧模型下 PTT 在 STANDBY 会自动召唤 chat（ADR-008 §2）。本 ADR 移除该自动召唤——用户必须显式按 BACK 进 CHAT 再说话。这一步看似多按一下键，但换来的是**PTT 行为可预测**：在 STANDBY 不会因为手抖按到 PTT 就跳到 CHAT 启动一轮空对话。

## 实施分阶段

四个阶段，各自独立可测、可回滚：

1. **Phase A — 状态机**：扩 [bb_radio_app.h](../../firmware/include/bb_radio_app.h) enum 到 4 态；在 [bb_radio_app.c](../../firmware/src/bb_radio_app.c) 加 `set_radio_app_state` 的 4-态版本；boot 路径按模式选初始态。**先不改 UI，验证状态转移日志正确。**
2. **Phase B — Standby UI**：在 [bb_lvgl_display.c](../../firmware/src/bb_lvgl_display.c) 加 LOCKED/UNLOCKED 两套 standby 渲染，底部按键提示条。
3. **Phase C — 按键路由 + PTT 路由按 §3 表对齐**：删除 ADR-008 的自动召唤逻辑、删除 90s idle 计时器、删除 `bb_ui_*_is_active()` 风格的 overlay 互斥仲裁，全部改为读 `s_app_state` 直接 dispatch。
4. **Phase D — Settings 裁剪**：删 Session 行及相关 fetch 代码。

每个阶段单独 commit，CHANGELOG 记一条。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 维持 ADR-007/008 的 overlay 模型，只删 Session 行 | 不解决"PTT 行为不可预测"和"三层叠加心智负担"的根本矛盾 |
| 保留 idle-exit 但延长到 5 分钟 | 加 magic number 不解决问题；用户真要离开按 BACK 是 1 秒的事 |
| local_home 也保留 LOCKED 态（无密语，OK/BACK 直接跳过） | 死代码路径；用户在 local_home 看到锁屏会困惑"为什么我也要解锁" |
| Session 行保留但藏到二级菜单 | 当前没有二级菜单基建；为这一行造一套 settings 子页过度设计 |

## 后果

### 正面

- 任意时刻只有一个页面前台，互斥规则消失（不再有 N×N overlay 矩阵）
- PTT 行为按状态严格分桶，可预测
- local_home 与 cloud_saas 的差异收敛到"是否经过 LOCKED 态"这一条，其它路径完全一致
- 删 Session 行后 [bb_ui_settings.c](../../firmware/src/bb_ui_settings.c) 减少 ~150 行（含 fetch task / async pipeline）

### 负面 / 风险

- 破坏性变更：用户习惯了 ADR-008 的"PTT 直接召唤 chat"，新版需要 BACK → PTT 两步
- 没有 idle-exit 后，CHAT 长期停留可能加速 OLED 老化（量化数据待 v0.4.2 实测）
- 状态机扩到 4 态，所有引用 `bb_radio_app_state_t` 的代码都要 review（grep 显示约 12 处）

### 未来工作

- 把 idle-exit 作为 Settings 可配项加回（off / 90s / 5min / 30min）
- LOCKED 态密语支持本地缓存（避免每次都走 cloud verify），降低 cloud_saas 启动时延
- STANDBY 屏支持"recent activity"上滑预览最近一次 CHAT 的尾部消息
- 4 态 enum 持久化到 NVS（断电重启恢复 last page），但需要先解决"重启时 cloud 还没连上 → 是否仍走 LOCKED"的语义
