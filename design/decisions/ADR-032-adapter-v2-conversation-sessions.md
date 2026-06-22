# ADR-032: adapter_v2 会话生命周期 —— 默认续接 / 新对话 / 历史选择 / 按会话隔离

- **日期**: 2026-06-23
- **状态**: 提议（P1 默认续接 + 设置页 PTT 退出已落地；P2 新对话/选择/会话重启、P3 历史隔离待实现）
- **关联**: ADR-014（逻辑会话：稳定 `ls-` id + 可换后端会话 id）、ADR-021/024（管家 workspace + 同源 worker）、ADR-027（设备端切 Home Adapter，cloud 组装的设备态选择器 —— 本 ADR 是它的「切对话」对应物）、ADR-028（Conversation Core v2 / Turn 模型）、ADR-031（OpenCode 作为 canonical 后端 —— 见下「与 ADR-031 的关系」）

## 背景

adapter_v2 用 **一根常驻 PTY 跑交互式 CLI（当前 claude）** 作为设备/语音线的后端（弃 `claude -p`，详见 [[adapter-v2-pty-refactor]] 方向）。CLI 自己管它的「对话」（claude：`~/.claude/projects/<cwd 编码>/<session-uuid>.jsonl`，一个对话一个文件）。但 adapter 此前**完全不管对话的生命周期**，暴露三个问题：

1. **每次重启都开新对话**。默认会话的 claude 裸启动（不带 `--continue`），所以 adapter 一重启就是空白新对话，用户上一轮聊的全断。真机测试期间 workspace 下因此堆了一大把一次性 `.jsonl`。用户诉求:**默认应该续上次对话**。
2. **没有「新对话」**。用户想主动开一段干净对话时无从下手。
3. **没有历史 + 有串台风险**。用户想看历史对话列表并选一个恢复；而一旦分了多对话，设备端的聊天记录缓存（`bb_chat_cache`，按 sid 分）和历史加载（`agent.messages`）**必须按会话隔离，绝不能串台**（A 会话显示 B 会话的记录）。

ADR-014 在 v1 已经定义了「逻辑会话」抽象（设备只见稳定 `ls-` id，后端会话 id 失效时透明换一个）。adapter_v2 是 PTY 模型，**复用 ADR-014 的逻辑层精神**，但「后端会话 id」从「`claude -p --resume` 的目标」变成「常驻 PTY 当前跑的 claude 对话 uuid」。

## 决策

### 核心模型

**一个「会话」= 一个 CLI 对话（claude 的 `<uuid>.jsonl`）。adapter 始终持有并上报「当前 active 会话 id」作为唯一真相。** 设备端的一切（聊天缓存、历史加载、选择器高亮）都按这个 id 对齐。

### 1. 切换 = 重启 PTY child（确定性，不用 CLI 交互 picker）

所有对话切换都是**杀掉当前 PTY child + 用不同 flag 重起**:

| 动作 | 启动 flag |
|---|---|
| 默认续接 | 持久化的 active id 存在 → `--resume <id>`；否则 `--continue`（续最近的） |
| 新对话 | `--session-id <新 uuid>`（全新对话） |
| 选历史 | `--resume <选中 uuid>` |

**为什么重启而不是 claude slash 命令**：`/resume` 会弹 claude 的交互式选择器（设备没法导航）、`/clear` 只清当前上下文不是真正分会话。重起 PTY 带显式 flag 是确定性的。重启复用两块已有基建:**云中继缓存 bridge 的「死会话驱逐重建」**（ADR-027 落地时为 SaaS 加的）—— 杀掉 `DefaultID` 会话后,下一轮 transcript 自动在新 PTY 上重建 bridge；web 终端断线自动重连。

**不用裸 `--continue` 当长期方案**:adapter 必须知道 active 对话 id（历史隔离要靠它对齐），所以新建/恢复一律用**显式 id**;boot 时读持久化的 active id → `--resume <id>`。

### 2. 持久化 active 会话 id

active 会话 uuid 持久化在 adapter 侧（与固件持久化 active adapter 同理，见 ADR-027 落地）。boot:有持久化值 → `--resume <id>`(精确续上次那一段,即使用户选过旧会话);无 → `--continue`(续最近)或全新。用户**新建/选择**会话时更新持久化值。

### 3. 历史列表（`agent.sessions`）

adapter 枚举 workspace 的 `.jsonl` → `[{id, 标题, lastUsedAt}]`（标题取首条用户消息），按时间倒序,回给设备做选择器。替掉 `internal/cloudrelay/proxy.go` 现在的空 stub。

### 4. 按会话隔离（防串台）

**单一真相 = adapter 上报的 active 会话 id**:

- adapter 在回包里上报当前 active id;设备聊天缓存（`bb_chat_cache`）按它 key。
- `agent.messages(sid)` **解析对应会话的 `.jsonl` 返回真实消息**（现在是空 stub）→ 设备加载的就是该会话历史。
- 切会话时:adapter active id 变 → 设备切缓存 + 重新 `agent.messages` 拉新会话历史。任何一环都按同一个 id 走,**绝不混**。

### 5. 设置页 PTT 退出（关联设备 UX）

设置页打开时按 PTT → 直接退回 chat（一个随手可达的「退出」大键），不再被静默丢弃。固件 `bb_radio_app` 在 PTT DOWN 边沿置请求标志 + 唤醒主循环,在与 nav 退出同一 task 上拆 overlay（LVGL + 状态转换串行化）。

## 分期

| 阶段 | 内容 | 状态 |
|---|---|---|
| **P1 默认续接** | 默认会话 claude 加 `--continue`（`ADAPTER_V2_RESUME=0` 可关） | ✅ 已落地（PR #234） |
| **PTT 退出** | 固件设置页 PTT 退出 | ✅ 已落地（PR #234） |
| **P2 新对话 + 选择 + 会话重启** | adapter 会话重启 + 持久化 active id + 显式 id 上报；固件菜单（新建 / 列表 / 选择） | 待实现 |
| **P3 历史隔离** | `agent.messages` 解析 `.jsonl` + 设备按 active id 隔离缓存 | 待实现 |

> P1 用 `--continue`（续最近）是过渡;P2 落地后改为「持久化 active id + 显式 `--resume <id>`」,与隔离模型一致。

## 取舍

- **切换=重启 PTY** 有一次「杀+重起 claude」的延迟（boot + warmup,几秒）。可接受:对话切换不是高频操作,且确定性远胜 scrape 交互 picker。
- **历史标题取首条用户消息**:claude 的 `.jsonl` 没有稳定的标题字段,首条用户消息是最朴素可靠的近似。
- **PTT 退出在 DOWN 边沿触发**:退出后 PTT 的 UP 边沿落在 chat 态,需保证无害(不误触发录音)。

## 与 ADR-031 的关系

ADR-031 把 v1 的「每 CLI 一个 scrape driver」收敛到「OpenCode 作为 canonical 后端（serve + SDK）」。adapter_v2 走的是**另一条线:PTY 跑交互式 TUI**(当前 claude)。本 ADR 的会话生命周期模型**与后端无关**——无论 PTY 跑 claude 还是别的 CLI,「一个会话=一个 CLI 对话 + adapter 持有 active id + 重启切换 + 按 id 隔离」都成立。若后续 adapter_v2 的后端换成 OpenCode serve,会话切换从「重起 PTY」变成「serve 的 session API」,但设备侧契约（agent.sessions / create / messages + active id 隔离）不变。
