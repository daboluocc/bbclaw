# ADR-028: Conversation Core v2 — 跟手交互架构（Turn 模型 / 打断 / 状态 / 语音对齐）

- 状态：已接受，实施中（2026-06-13）。M1（turn.cancel 协议 + Interrupt(sid) + 打断备注注入）
  与 M2 的采样率幂等已落地（adapter `7825589` + firmware `9e52610`，v0.5.6）；
  M3（conv_core）/M4（全 WS）/M5（UI 瘦身）待排期。
- 取代/收编：ADR-009（agent 状态机，转移表保留、装饰态降级）、`design/STATE_MACHINE.md` 与 `design/AGENT_STATE_MACHINE.md` 中与本文冲突的部分
- 关联：ADR-002（会话生命周期）、ADR-012（固定页菜单，保留）、ADR-017（TTS reading mode，字幕机制升级）、ADR-021-firmware-ui（保留 Task List）、ADR-027（messageId echo 先例）

## 1. 问题：为什么"不跟手"

2026-06 对固件 27k 行 C 代码 + adapter 协议的全量审计结论——交互不跟手不是单点 bug，
而是四个结构性缺陷叠加：

### 1.1 打断（barge-in）链路五层断三层

| 层 | 现状 | 证据 |
|---|---|---|
| 按键捕获 | ✅ ~10ms debounce | `bb_ptt.c` |
| 本地 TTS 播放 | ⚠️ 软中断轮询，~60ms 粒度；**取消时 TTS 队列不 drain（泄漏）** | `bb_ui_agent_chat.c` tts_cancel_in_flight |
| Agent 流处理 | ❌ PTT 按下不取消 in-flight turn，仅 BACK 键可取消 | `bb_radio_app.c:1987` |
| cloud_wait 等待 | ❌ `bb_adapter_stream_finish_stream()` 阻塞最长 90s，期间 PTT 被静默吞掉，只回错误震动 | `bb_radio_app.c:843-848` |
| 协议 | ❌ **local/cloud 均无 cancel/abort 帧**；设备停播后 adapter 仍持续推 reply.delta + tts.chunk | `bb_adapter_client.c` 全文无 cancel |

adapter 端 `drv.Stop(sid)`（SIGTERM CLI）**已存在**（`adapter/internal/httpapi/agent.go:313`），
只是设备没有任何协议途径触发它。

### 1.2 三重状态机并存，手工同步

- `bb_state`（Phase 4.9 SSoT，转移表驱动，设计良好）
- `s_app_state`（bb_radio_app.c 旧枚举，与 `bb_page_t` 重叠）
- `s_chat.state / s_chat.active / s_chat.sending`（bb_ui_agent_chat.c 自留）

加上 `post_state()` 在 UI 层 20+ 处直接调用与 bb_state listener 双轨并发，
顺序不保证 → "buddy 卡 thinking"、"播完还显示 speaking" 类 bug 反复出现。

### 1.3 stream_task 巨型阻塞循环

一个 40KB 栈的 task 串行做：PTT 轮询 → VAD → Opus 编码上传 → **阻塞等云（90s 硬超时）**
→ 同步 TTS 播放。等待期间整机对输入"失聪"，打断在架构上不可能干净实现。

### 1.4 语音-文本不对齐（双通路分裂）

- 通路 A（PTT 语音路，`bb_radio_app.c:1001-1143`）：TTS chunk 自带 `tts_text` 字段但**从未使用**，
  无字幕、无逐句高亮；ASR 全文一次性上屏，用户不知道当前在念哪句。
- 通路 B（Agent Chat 路，`bb_ui_agent_chat.c`）：设备端自行分句 + `post_subtitle()`，有对齐。
- 两条通路各一套 TTS 播放代码；chunk 可声明任意采样率，逐 chunk 触发 I2S
  关→重配→开（~50-100ms 卡顿）。

### 1.5 臃肿清单（审计确认）

| 项 | 位置 |
|---|---|
| 三套主题（buddy-ascii / buddy-anim / text-only）各自复制颜色与布局宏 | `bb_theme_*.c` |
| `cycle_driver` vs `set_active_driver` 100+ 行近似复制 | `bb_ui_agent_chat.c:2243-2434` |
| TTS 开关在 Settings 与 CHAT 各持一份，改后不同步 | `bb_ui_settings.c` / `bb_ui_agent_chat.c` |
| HEART/CELEBRATE/ATTENTION 装饰态混入核心状态机（9 态） | `bb_agent_theme.h` |
| 本地传输逐帧 HTTP POST + TTS base64（+33% 开销），NDJSON 无 messageId | `bb_adapter_client.c` |
| Settings 驱动列表同步等 WiFi 60s 才显示 | `bb_ui_settings.c` driver_fetch_task |
| session picker 残留字段、buddy-anim 空 topbar 实现等死代码 | 各处 |

## 2. 决策总览

围绕一个核心抽象重建会话层：**Turn（回合）**。

> 任何时刻，设备最多只有一个"当前 turn"。PTT 按下永远有效：
> 它原子地取消当前 turn（本地停声 + 协议 cancel），并立即开启新 turn 的收音。
> 所有下行事件（text delta / tts chunk / 状态）都带 `turnId`，
> 非当前 turn 的事件在入口处被原子丢弃并释放。

这一条规则同时解决：打断、状态错乱、过期数据串扰、"cloud_wait 失聪"。

### 2.1 Turn 模型

```
turn_id：uint32，设备端单调递增，随 voice.stream.start / agent message 上行
生命周期：CAPTURE → UPLOAD → WAIT → REPLY(text+tts 流式) → DONE | CANCELLED | TIMEOUT
```

- 打断 = `cancel(turn_id)` + `turn_id++`，旧 turn 的一切异步产物按 id 过滤丢弃。
  设备**不等待** cancel 回包即可开始新 turn（fire-and-forget），干净与否由 id 过滤保证。
- turn 级超时：WAIT/REPLY 阶段连续 15s 无任何下行事件 → 本地判 TIMEOUT，
  状态进 ERROR 并立即可重试。废除 90s 阻塞死等。

### 2.2 任务拓扑（替换 stream_task 巨循环）

```
conv_core  (P6, core0)  状态机 + turn 编排。唯一写状态者。只消费事件队列，永不阻塞 IO。
audio_in   (P7, core0)  I2S RX → ring → Opus 编码 → 交 net_io。仅受 conv_core 启停控制。
net_io     (P5)         唯一网络 task：WS 收发（local 与 cloud 统一走 WS）。
                        下行帧解析为事件（带 turnId）投递 conv_core / audio_out。
audio_out  (P6, core0)  TTS sink：消费 chunk{turn_id,seq,pcm,text} 队列 → I2S TX。
                        每个 chunk 开播前发 SUBTITLE 事件；收到 CANCEL 立即停写+drain+flush。
LVGL       (P4, core1)  纯渲染。订阅 conv_core 状态快照 + transcript model，不持业务状态。
```

事件队列 `conv_evq` 是 conv_core 的唯一输入：按键、VAD、网络事件、播放进度/完成、
超时 timer。bb_state 的**声明式转移表 + 不变量检查保留**（这部分设计是对的），
但 dispatch 改为在 conv_core 内同步执行，不再经 `lv_async_call` 绕道 LVGL task。

**删除**：`s_app_state`、`s_chat.state/sending`、`post_state()` API、
stream_task 内 `arming/streaming/session_busy` 本地标志。UI 一律读快照。

### 2.3 打断：四层全部落地

| 层 | 设计 | 延迟预算 |
|---|---|---|
| L1 输入 | 转移表新增规则：`{*, BUSY|SPEAKING|WAIT, PTT_DOWN} → cancel_turn + LISTENING`。任何状态 PTT 都有效，废除 cloud_wait 吞键 | 按下→状态切换 < 20ms |
| L2 音频 | audio_out 收 CANCEL：停止当前 write（播放块从 256-sample 降到 64-sample，≈4ms 粒度）→ drain 队列并释放 chunk → `i2s_channel_disable` + DMA flush → 回 PLAYBACK_STOPPED 事件 | 按下→静音 < 50ms |
| L3 协议 | 新帧 `turn.cancel {turnId}`（见 §2.5）。adapter 收到后：中断 finish 流写出、`drv.Stop(sid)`、终止 TTS 合成、回 `turn.cancelled` | 不阻塞新 turn |
| L4 UI | 当前朗读句标记"已打断"（截断样式），transcript 保留已播部分；状态立即显示 LISTENING | 与 L1 同步 |

边界（明确不做）：**语音打断（说话即打断）暂不支持**。bbclaw 硬件
INMP441 + MAX98357A 为半双工共用 I2S，无 AEC；播放时无法可靠收音。
打断触发源 = PTT 键。ES8311 全双工板未来可加 VAD barge-in，协议已就绪。

### 2.4 语音-文本对齐：sink 驱动字幕

对齐的唯一可靠时钟是**实际播放进度**，因此字幕由 audio_out 驱动，不由网络到达驱动：

1. 协议规定每个 `tts.chunk` 必带 `{turnId, seq, text, sampleRate}`；
   **首 chunk 的 sampleRate 对全 turn 生效**，设备每 turn 只配一次 I2S 时钟
   （默认预配 24kHz，消灭逐 chunk 重配）。
2. audio_out 在 chunk N 写入 I2S 前发 `SUBTITLE{turn_id, seq, text}` →
   UI 在 transcript 中高亮该句（全文仍流式显示，"当前朗读句"高亮即对齐）。
3. **删除 PTT 路同步 TTS 播放代码**，两条通路统一走 audio_out 一份实现；
   设备端分句逻辑（Agent Chat 的 `extract_pending_chunk`）随之退役——分句由
   adapter TTS 按句合成保证。
4. 上行新增 `asr.partial`（说话时实时上屏灰字，`asr.final` 转正），提升"在听"的跟手感。

### 2.5 协议 v2（local 与 cloud 同构）

local_home 从逐帧 HTTP POST 升级为 WS（adapter 已有 WS 基础设施；HTTP 仅留控制面/配置）。
上行音频与下行 TTS 一律二进制帧，去 base64。所有 JSON 帧必带 `turnId`（控制面带 `messageId`，
沿 ADR-027 echo 规则）。

新增/修订帧：

| 帧 | 方向 | 语义 |
|---|---|---|
| `turn.start {turnId, sessionKey, codec, sampleRate}` | 设备→ | 开 turn，替代 stream/start |
| 二进制音频帧（首字节 type + turnId + seq） | 设备→ | 上行 Opus |
| `turn.commit {turnId}` | 设备→ | PTT 松开，替代 stream/finish；**不阻塞**，回复经事件流 |
| `turn.cancel {turnId}` | 设备→ | P0。adapter：停 ASR/LLM/TTS，回 `turn.cancelled` |
| `asr.partial / asr.final {turnId, text}` | →设备 | 实时转写 |
| `reply.delta {turnId, text}` | →设备 | agent 文本流 |
| `tts.chunk`（二进制，头含 turnId/seq + text 副信道）/ `tts.done {turnId}` | →设备 | 见 §2.4 |
| `turn.state {turnId, phase}` | →设备 | thinking / tool_running / synthesizing 等细粒度状态 |
| `flow.credit {n}` | 设备→ | 背压：设备按 audio_out 队列水位发信用，adapter 据此节流 TTS 推送 |

Cloud relay 改动：透传 `turn.cancel`、`turn.state`、`asr.partial`、`flow.credit`
四类新 kind（按 CLAUDE.md 跨组件同步表，cloud hub 路由需同步加 kind 分发）。

### 2.5.1 `turn.cancel` 的 adapter 端语义：杀掉 in-flight `claude -p` 进程

claude-code 驱动的事实（`adapter/internal/agent/claudecode/driver.go`）：

- 每个回合就是一个独立子进程：`claude -p <text> --output-format stream-json
  [--resume <id>]`，经 `exec.CommandContext` 启动，`cancel()` 即杀进程。
- 因此 **PTT 打断 = 终止当前回合的整个后台执行**——LLM 推理、正在跑的
  bash/工具调用、MCP 调用随进程一起终止。这不是副作用而是期望语义：
  用户按 PTT 既表示"别说了"，也表示"别做了"。
- `resumeID` 在回合开始即落定（`--session-id` / init 事件），且 Claude Code
  增量持久化会话 JSONL —— **中途被杀的回合仍可 `--resume`**，恢复加载时
  能看到被截断的部分 assistant 输出。

需要新增/修改：

1. **`Interrupt(sid)` 驱动方法**（区别于现有 `Stop(sid)`）：现有 `Stop` 会
   `delete(d.sessions, sid)` + `close(s.events)`，把逻辑会话整个销毁，对打断
   过重。`Interrupt` 只 `s.cancel()` 杀当前回合子进程，**保留 session 与
   resumeID**，使下一回合继续 `--resume` 同一对话。终止顺序：SIGTERM →
   2s 宽限 → SIGKILL（设置 `cmd.Cancel` / `WaitDelay`，给 CLI 机会 flush
   JSONL）。被杀回合向设备 emit `turn.cancelled {turnId}` 后正常 `EvTurnEnd`。
2. **打断记录（resume 可见）**：设备发 `turn.cancel` 时附带播放进度
   `{playedSeq, playedText}`（audio_out 知道实际播到哪句）。adapter 把打断
   作为会话事实记两处：
   - adapter 会话历史/chat cache 记 `turn.interrupted` 事件（设备 transcript
     显示截断标记的数据来源）；
   - **下一回合的 prompt 前注入打断上下文**，随 `claude -p --resume` 带给模型，
     形如：`[系统提示：你上一条回复在「<playedText 末句>」处被用户按键打断，
     其后的内容用户没有听到；若有正在执行的任务已被终止。]`
     这样 resume 后的 AI 明确知道发生过打断、用户听到了多少、执行被截断在哪，
     而不是把半截输出当作已完整送达。
3. **边界**：`turn.cancel` 杀的是**当前回合**的 CLI 进程。butler 经
   `mcp__bbclaw__dispatch` 派发出去的 worker 任务是独立进程，不随回合进程
   终止（dispatch cancel 仍为 §2.5 表外的 P2，需 OpenClaw gateway 配合）；
   打断注入的上下文应说明 dispatch 任务仍在后台运行（如有），由 butler
   下回合自行汇报。

### 2.6 状态：6 核心态 + 装饰事件

核心状态机收敛为 6 态：`SLEEP / IDLE / LISTENING / THINKING / SPEAKING / ERROR`。

- `BUSY→THINKING` 改名以与 UI 语义一致；`turn.state` 细分阶段（tool_running 等）
  作为 THINKING 的子标签展示，不进转移表。
- HEART / CELEBRATE / ATTENTION 从状态机移除，降级为**装饰事件**
  （一次性动画 overlay，播完自动回落，不参与转移、不会"卡住"）。
- UI 渲染唯一来源 = conv_core 状态快照（seq lock 读）；listener 通知 + LVGL 每 500ms
  对账兜底，结构性消灭"播完还显示 speaking"。
- DIZZY→ERROR 保留 3s 自愈；新增最近 64 次转移 ring buffer（`bb_state_log`）供排障回放。

### 2.7 UI / 菜单瘦身

保留（审计确认设计良好）：chunk coalesce 双缓冲、scroll worker、history 分页 lazy-load、
固定三页导航 + Task List（ADR-012/021）、`UI_DESIGN_LANGUAGE.md` token 体系。

砍掉/重构：

1. 主题只留 **buddy-anim**；buddy-ascii 移入 simulator 专用，text-only 删除。
2. 颜色/布局宏收口到 `bb_ui_theme.h` 单一源（消灭三份拷贝）。
3. `cycle_driver` / `set_active_driver` 合并为 `apply_driver_switch(name_or_delta)`。
4. TTS 开关收口到单一 holder（NVS + 事件广播），Settings 改值 CHAT 即时生效。
5. Settings 列表改"缓存即显 + 后台刷新"（启动时持久化上次驱动/模型列表），
   打开菜单秒出，不再等 WiFi 60s。
6. 删：buddy-anim 空 topbar 实现、session picker 残留字段、
   adapter picker 非 cloud_saas 路径编译（运行时 gate 收紧）。

## 3. 迁移计划（每阶段独立可发版，OTA 安全）

| 阶段 | 内容 | 跨组件 |
|---|---|---|
| M1 协议 | adapter + firmware 实现 `turn.cancel`（含 `Interrupt(sid)` 杀回合进程 + 播放进度上报 + 打断上下文注入下一回合，见 §2.5.1）、tts.chunk 元数据（turnId/seq/text/首帧采样率）、`turn.state`；旧帧保留兼容 | cloud relay 透传新 kind |
| M2 音频 | audio_out 统一 sink；删 PTT 路同步播放；字幕对齐；固定 24kHz；64-sample 中断粒度；cancel 时 drain+flush | 无 |
| M3 核心 | conv_core 替换 stream_task 巨循环；废除三重状态机与 post_state；6 态收敛；PTT 全状态可打断 | 无 |
| M4 传输 | local_home 升级 WS + 二进制帧 + flow.credit；asr.partial | adapter WS server、cloud relay |
| M5 瘦身 | 主题合一、driver 切换合并、TTS holder、Settings 缓存化、死代码清理 | 无 |

M1+M2 落地后用户即可获得"按键即打断、字幕跟读"；M3 解决状态正确性；
M4/M5 是延迟与可维护性收益。每阶段固件 + adapter 同 tag 发布（单 tag 双产物策略）。

## 4. 验收指标

| 指标 | 现状 | 目标 |
|---|---|---|
| PTT 按下 → TTS 静音 | 不定（cloud_wait 期间为 ∞） | < 50ms，任何状态 |
| PTT 按下 → 可重新说话 | 需等 turn 结束 | < 100ms |
| 打断后旧 turn 残留（错字/串音/泄漏） | 队列不清、协议继续推流 | 0（turnId 过滤 + drain） |
| 状态显示与实际不符窗口 | 可长期卡死 | < 500ms（对账兜底） |
| 字幕与播放句对齐 | PTT 路无字幕 | 逐句高亮，两通路一致 |
| 等云无响应的死等 | 90s 硬阻塞 | 15s 判超时，立即可重试 |
| 逐 chunk I2S 重配 | 每 chunk 可能 ~50-100ms | 每 turn ≤ 1 次 |
