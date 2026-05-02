# ADR-012: 固定三页菜单（Chat / Settings）取代 overlay 召唤模型

- **编号**: ADR-012
- **标题**: 把 overlay 召唤模型重构为三个固定页面 + 显式三态状态机
- **日期**: 2026-04-30（2026-05-03 修订：删除 STANDBY，CHAT 成为主页）
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

### 1. 三态显式页面状态机

```c
typedef enum {
  BBCLAW_STATE_LOCKED   = 0,  /* cloud_saas only: 待密语解锁 */
  BBCLAW_STATE_CHAT     = 1,  /* 默认主页（Agent Chat） */
  BBCLAW_STATE_SETTINGS = 2,  /* Settings 页面 */
} bb_radio_app_state_t;
```

**初始态选择**（开机 / boot）：

| Transport 模式 | 初始态 | 理由 |
|---|---|---|
| `cloud_saas` | `LOCKED` | 保留密语解锁能力（沿用现有 `passphrase_unlock_enabled()` 谓词） |
| `local_home` | `CHAT` | 本地模式无密语机制，开机直达 CHAT 主页 |

> STANDBY 页面已删除（2026-05-03）。CHAT 直接作为主页，解锁后/启动后直接进入，无需中间页。

### 2. 状态转移图

```
                 ┌─────────────┐
   (cloud)  ───→ │   LOCKED    │
                 └─────────────┘
                       │  PTT-talk + 密语验证通过
                       ▼
   (local)  ───→ ┌─────────────┐ ←──── BACK ──── ┌──────────┐
                 │    CHAT     │                  │ SETTINGS │
                 │  (默认主页)  │ ──── OK ──────→ └──────────┘
                 └─────────────┘
```

**关键不变量**：

- 任何时刻**只有一个**页面在前台（全屏），不再叠加 overlay。
- `LOCKED` 态在 `local_home` 模式下永不进入。
- CHAT 是主页，无 idle-exit。SETTINGS 通过 BACK 返回 CHAT。
- CHAT 的 BACK 键仅用于取消 in-flight turn，不退出页面。

### 3. 按键路由表（Flipper 6-button，依 ADR-006）

| 页面 | UP | DOWN | LEFT | RIGHT | OK | BACK | PTT |
|---|---|---|---|---|---|---|---|
| **LOCKED** (cloud) | ignore | ignore | ignore | ignore | ignore | ignore | **按住录音 → 松开走密语验证**（复用现有 `BB_STATUS_VERIFY` 流程） |
| **CHAT** | 滚动历史 | 滚动历史 | 上一 driver | 下一 driver | → SETTINGS | 取消 in-flight（空闲时忽略） | 按住录音 → 松开发送 |
| **SETTINGS** | 上移行 | 下移行 | 值预览(-) | 值预览(+) | 提交并落 NVS | → CHAT (放弃未提交预览) | ignore |

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

PTT 按下 → 状态条切到 `BB_STATUS_VERIFY_TX`，松开 → `BB_STATUS_VERIFY` 处理中 → 成功转 CHAT，失败 `BB_STATUS_VERIFY_ERR` 后回 LOCKED。

### 5. CHAT 中途按 BACK 的语义

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

- CHAT 是主页，没有"退出"的目标页面。
- 真机反馈：90s 太短，用户思考下一句就被打断。

代价：CHAT 页面停留时间不限。OLED 老化 / 功耗成本由用户自己控制。后续若有数据支持，可作为 Settings 一行加回（"chat idle timeout: off / 90s / 5min"）。

### 8. CHAT 中途按 BACK 的语义

CHAT 处于 BUSY / SPEAKING（agent 正在生成或 TTS 在播）时，用户按 BACK 的行为：

**取消正在进行的回合，留在 CHAT 页面。**

实现要点：
- 调 `bb_ui_agent_chat_cancel()` 取消 in-flight turn
- 已经发送但未消费的 stream 帧丢弃
- 已上传的 ASR 音频不退（云端配额已扣，无法回滚）
- 空闲时按 BACK 无操作（CHAT 是主页，无处可退）

### 9. PTT 路由收敛

| 状态 | PTT 行为 |
|---|---|
| LOCKED | 密语验证录音 |
| CHAT | 录音 / 发送 |
| SETTINGS | 忽略 |

## 实施分阶段

已全部完成（2026-05-03）：

1. **Phase A — 状态机**：[bb_radio_app.h](../../firmware/include/bb_radio_app.h) enum 改为 3 态（LOCKED/CHAT/SETTINGS）。
2. **Phase B — 删除 STANDBY UI**：[bb_lvgl_display.c](../../firmware/src/bb_lvgl_display.c) 删除 STANDBY view（品牌 logo、时钟、吉祥物动画、idle timeout）。
3. **Phase C — 按键路由**：CHAT 的 OK → SETTINGS，BACK 仅取消 in-flight；SETTINGS 的 BACK → CHAT。
4. **Phase D — Settings 裁剪**：删 Session 行及相关 fetch 代码（已在之前完成）。

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
- 删除 STANDBY 后导航更直接：解锁/启动 → 直接进入 CHAT，减少一步操作

### 负面 / 风险

- 没有 idle-exit 后，CHAT 长期停留可能加速 OLED 老化（量化数据待实测）
- 3 态状态机意味着没有"安静"的待机屏，CHAT 的 SLEEP 态承担待机视觉

### 未来工作

- 把 idle-exit 作为 Settings 可配项加回（off / 90s / 5min / 30min）
- LOCKED 态密语支持本地缓存（避免每次都走 cloud verify），降低 cloud_saas 启动时延
- 3 态 enum 持久化到 NVS（断电重启恢复 last page），但需要先解决"重启时 cloud 还没连上 → 是否仍走 LOCKED"的语义
