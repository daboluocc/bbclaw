# ADR-042 — adapter_v2 便捷口令路由 + 定时提醒 / 主动通知

- **状态**: Proposed（P0 = 口令路由 + 一次性提醒 + 通知 outbox；P1 = 周期汇报 / 勿扰；P2 = 外部 bridge）
- **日期**: 2026-06-30
- **关联**: ADR-035（v2 走交互式 PTY 抓屏，弃 `claude -p`），ADR-040 / ADR-041（回合同步 / 取消），ADR-034（CLI 子任务列表通道——**与本 ADR 的「提醒任务」是两回事，见 §6 命名**），ADR-032（v2 对话会话）
- **依据**: `chat/2026-06-30-cc-connect-adapter-enhancement-research.md`（对 chenhg5/cc-connect 的借鉴调研）

## 0. 背景与目标

cc-connect 把本机 Agent 接到聊天平台，强项是 **slash command 体系** 和 **timer/cron 主动任务**。BBClaw 不追求兼容更多 IM 通道（硬件主控制面留在 adapter），但要借鉴两点直接服务硬件体验：

1. **便捷口令**：让「停止 / 状态 / 新对话」这类短命令**不进 LLM、不计费**，像自然语言快捷键。v1 有 `internal/voicecmd`（精确短语表→slash），**v2 至今没有迁过来**——这是回归缺口。
2. **定时提醒 + 主动汇报**：用户明确要「类似短信提醒」的主动呼叫——到点了 adapter **不等用户 PTT**，自己起一个 turn 把结果播给设备。

非目标（本 ADR 不做）：兼容第三方 IM channel；自然语言任意时间解析（P0 限定表达式）；周期 cron（P1）。

## 1. 决策总览

| 模块 | 包 | P 级 | 职责 |
|------|----|----|------|
| 命令路由 | `internal/command` | P0 | transcript → 结构化 `Intent`；精确短语优先，时间表达式轻量解析 |
| 调度器 | `internal/reminder` | P0 | `timer.once` 存储 + 到点触发；JSON 落盘 |
| 通知 outbox | `internal/notify` | P0 | 主动结果可靠投递（在线推屏+TTS，离线持久化重连补投，带 id 去重 + ack） |
| 周期汇报 | `internal/reminder`(扩展) | P1 | `report.cron` + 勿扰 / 失败降噪 |
| 外部 bridge | — | P2 | 只暴露 command/task/notification，不暴露硬件底层协议 |

## 2. 命令路由（P0）

### 2.1 拦截位置
口令拦截发生在 **ASR 之后、PTY 注入之前**——在 `deviceapi.Bridge.SubmitVoiceTurn` 入口：

```
PTT audio ─ASR─▶ transcript ─┬─ command.Parse 命中 ─▶ 执行 Intent（不进 PTY）
                             └─ 未命中 ─▶ 原 PTY 注入（喂 CLI，正常计费 turn）
```

理由：若把「停止」当普通文本注入 claude，会变成一句 prompt → 触发一次真实计费 turn，语义也错。必须在注入前短路。

### 2.2 Intent 结构
```go
type Intent struct {
    Kind   string            // turn.cancel / session.new / status.show / reminder.create / reminder.list ...
    Text   string            // 原始 transcript
    Source string            // voice / http / cloud
    Args   map[string]string // 如 reminder.create: {"delay":"30m","prompt":"检查烧录日志"}
}
```

### 2.3 P0 口令集
| Kind | 口令示例 | 行为 | v2 落点 |
|------|----------|------|---------|
| `turn.cancel` | 停止 / 取消 / stop | `Bridge.Interrupt()` | 已有 |
| `session.new` | 新对话 / 重新开始 / new | 注入 CLI 的 `/clear`（slash 命令非 LLM turn） | 新增 |
| `status.show` | 状态 / 现在怎样 / status | 组装 driver/session/cloud 状态串 → `Bridge.speak` | 新增 |
| `reminder.create` | （X 分钟后 / X 小时后 / 明天 HH:mm）+ 提醒…… | 建 `timer.once` → speak 确认 | 新增 |
| `reminder.list` | 看提醒 / 有哪些提醒 | 列出待触发 → speak | 新增 |

### 2.4 解析策略（顺序短路）
1. **精确短语表优先**（移植 v1 voicecmd），保「停止」绝不误进 LLM、绝不误判时间。
2. **时间表达式轻量规则**：P0 只认 `X 分钟后` / `X 小时后` / `明天 HH:mm` / `每天 HH:mm`（每天留给 P1）。命中即抽 `delay`/`runAt` + 剩余文本作 `prompt`。
3. **模糊自然语言 = P1**：交 Butler 生成结构化 intent，高风险任务二次确认。
4. **单一执行器**：voice / HTTP(adminapi) / cloud relay 都调同一 `command.Router.Dispatch(Intent)`，不在三处各写分支。

## 3. 定时提醒（P0：只做 `timer.once`）

```go
type Reminder struct {
    ID        string    // "rem_..."
    Kind      string    // "once"（P0）/ "cron"（P1）
    Title     string
    Prompt    string    // 到点喂给 Butler/Agent 的指令
    RunAt     time.Time // once
    Target    Target    // deviceID / sessionID / cwdName / driver
    State     string    // scheduled / running / done / failed / canceled
    CreatedAt time.Time
}
```

- Store：`$BBCLAW_DATA_DIR/reminders.json`（与 settingsstore/projectstore 同款 JSON 落盘）。
- 触发：到点 `reminder` 起一个 **Butler turn**（走 v2 既定的交互式 PTY 路线，订阅内计费——**不得退回 `claude -p`**，见 ADR-035），结果交给 §4 notify。
- **触发归属（2026-06-30 决定）**：提醒**回到创建它的目标会话**触发（`Reminder.Target` 在 create 时记录 deviceID/sessionID/cwdName）。Scheduler 通过 `Injector` 把 prompt 注入该设备**当前活着的 Bridge**（等价于 adapter 替用户说了一句）。Bridge 是按连接生命周期创建的（`devicews` / `cloudrelay` 各自 `deviceapi.New`），故需要一个**按 deviceID 索引的 bridge 注册表**：连接建立时注册、断开时注销。
  - 目标设备/会话**离线**：不丢——转入 §4 notify outbox，重连后补投（与「主动通知」同一条离线补投路径）。
- P0 先只 `once`：最贴语音设备、最易端到端验证。周期 `cron` 留 P1。

## 3.1 主动推送优先级（2026-06-30 决定）—— **Cloud 优先，LAN 暂缓**

「服务端主动推 TTS」要把回合模型从纯 PTT 发起扩展出「服务端发起」一条主动帧。两条路都走 WebSocket（非 HTTP），但**先只做 cloud_saas 路径，LAN 直连暂不实现**：

- **优先 cloud**：真实出货设备走云端配对；先把 cloud 链路打通。
- **LAN 暂缓**：`devicews` 的本地直连主动帧留到云路径验证后再补（同一套设备协议帧，复用即可）。
- cloud 主动推送要补：① reminder 记 `Target.DeviceID`（create 时从 curdevice 拿）；② relay 暴露 **relay 级 `Send(Envelope)`**（底层单 conn 的 write 已存在）；③ 新增 server→device 主动 envelope kind `notify.*`；④ **云端 hub 路由 adapter→device 主动帧**（跨仓库 bbclaw-reference cloud，先部署云侧再发固件——CLAUDE.md 协议同步表）；⑤ 固件侧 `notify.*` 处理（亮屏/展示/播 TTS/ack）。
- 离线：cloud WS 断或设备离线 → outbox 缓存重连补投（§4）。

## 3.2 主动 TTS 播报（在视觉通知之上，2026-06-30）—— **固件拉取式，云端零改动**

视觉通知（§3.1）只弹 toast。要让设备到点**主动念出来**，关键约束（实测云/固件代码）：

- cloud_saas 下 **TTS 音频走「设备发起的流式 HTTP 请求」**（设备 PTT→POST transcript→NDJSON 流里夹 `tts.chunk`），控制 WS 只走 `session.notification` 等控制帧。**没有**服务端→设备的常驻音频推送通道。
- 因此主动 TTS 用**固件拉取式**最自然：通知(WS)带 `ttsText`/`speak` → 固件收到后**自己发 HTTP** 到云现有 `POST /v1/tts/synthesize`(`{text,deviceId,codec:pcm16}`→音频)→播放。

**三端改动（决定）**：
- **adapter_v2**：`session.notification` payload 加 `ttsText` + `speak:true`（reminder 到点带上）。
- **cloud**：**零改动**——hub 透传 payload 新字段；`/v1/tts/synthesize` 已存在。
- **固件**：`bb_adapter_client.c` 的 `session.notification` 分支：除现有 toast 外，若带 `speak` → 调 `/v1/tts/synthesize` 取 PCM16 播放（复用现有 HTTP+I2S 播放栈）。对话/录音中收到先入队不打断。**需 OTA 烧录验证**。
- 备选（未选）：云端对通知主动 TTS 后推音频——但音频不在控制 WS 上，需新增推送通道，比固件拉取重。

## 4. 通知 outbox（P0）

把「主动结果投递」从「设备显示任务」里抽出来——显示只是投递方式之一，后续还可投 Cloud/Web/admin。

```go
type Note struct {
    ID       string   // "note_..."（跨重连去重键）
    Kind     string   // "reminder.report" / "task.report"
    Title    string
    Body     string
    TTSText  string
    Severity string   // info / warn / error
    DeviceID string
    State    string   // pending / delivered / acked
    CreatedAt time.Time
}
```

投递规则：
- 设备在线：立即推屏 + 可选 TTS（复用 `Bridge.speak`）。
- 设备离线：留 outbox，重连后拉取。
- 去重 + ack：同 `note.ID` 只投一次；设备 ack 或 admin 标记已读后置 `acked`。
- 对话中收到：先入队、底栏未读数，不打断当前 turn。

## 5. 安全边界（主动任务比对话更敏感——它在用户不盯着时跑）

- P0 **只允许 `prompt` 型只读任务**，禁止直接 `exec` 任意 shell。
- 每个任务绑**已授权 cwd pool / project allow-list**（projectstore）。
- 每任务 timeout（默认 2–5 分钟），超时以失败通知收尾。
- 输出做摘要 + 截断，详情进 session history。
- 写文件 / commit / push / 删除等 destructive 操作：默认不做，需设备确认或 admin opt-in。
- 通知投递受 quiet hours / 勿扰 控制（P1）。
- Cloud relay 只透传 command/task/notify envelope，**权限决策仍在本地 adapter**。

## 6. 命名约定（避免与 ADR-034 冲突）

ADR-034 的 "task-list channel" 指 **CLI 自己派发的子任务列表显示**（屏幕上的 `⏺` 进度行）。本 ADR 的「定时任务」是**用户设的提醒**，语义完全不同。

**约定**：本 ADR 一律用 `reminder`（一次性）/ `report`（周期）/ `note`（通知），**不复用 `task` 一词**，设备协议 envelope kind 也用 `reminder.*` / `notify.*`，杜绝两套 task 混淆。

## 7. 落地路线

- **M1（本 ADR 首个 commit）**：`internal/command` + 移植 voicecmd + 接 `SubmitVoiceTurn`，支持 `turn.cancel` / `session.new` / `status.show`。
- **M2**：`internal/reminder`（timer.once）+ JSON store + 到点起 Butler turn。
- **M3**：`internal/notify` outbox + 设备在线推屏/TTS + 离线补投 + admin 列表。
- **M4（P1）**：`report.cron` + 勿扰 + 失败降噪。
- **M5（P2）**：可选外部 bridge。

## 8. 验收场景

- **口令**：设备说「状态」→ 不进 LLM，直接 speak 当前 driver/session/cloud 状态。
- **提醒**：说「30 分钟后提醒我检查烧录日志」→ speak「已设置」→ 到点 adapter 主动起任务 → 设备收通知 + 可选 TTS。
- **勿扰（P1）**：「今晚别提醒」→ 任务仍跑，只入通知中心不播 TTS。

## 9. 风险

- 自然语言时间解析易误解 → P0 限表达式 + 高风险二次确认。
- 主动任务可能触危险操作 → 默认只读报告。
- 设备离线 + cloud 重连致重复通知 → `note.ID` 去重 + ack。
- 周期任务长期累噪 → 静默 / 失败降噪 / 取消口令（P1）。
