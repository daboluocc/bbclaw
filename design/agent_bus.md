# Agent Bus 架构设计

> 状态：草案（Draft）
> 日期：2026-04-25
> 相关 ADR：[ADR-001 adapter 作为 Agent 总线](decisions/ADR-001-adapter-as-agent-bus.md)

## 1. 背景与目标

bbclaw 硬件需要能和多种 AI CLI 工具对话（Claude Code、Codex、Aider、Gemini CLI、本地 Ollama 等），而不是被某一家的产品协议绑死。

已评估并排除的路径：

| 路径 | 为什么不选 |
|---|---|
| Claude desktop BLE buddy | 官方协议**单向只读**（设备只能读 Claude 输出和回权限决定），**无用户消息注入**通道；developer-mode-only；Anthropic 专属，无法泛化到其他 agent |
| 设备直连云 LLM | 需要在 ESP32 固件里塞多家 SDK 和鉴权；CLI 每次版本升级都要重烧固件；没法利用用户桌面已有的登录态 |

**本设计选定的路径**：bbclaw-adapter（桌面常驻 Go 程序）做"Agent 总线"，向上统一暴露一个 `AgentDriver` 接口，向下把帧转成对应 CLI 的 stdin/stdout。

### 目标

1. 设备端写**一套** UI/UX 代码，对接任意 agent
2. 新增 agent 只改 adapter，**不动固件**
3. 设备↔adapter 帧协议沿用 buddy 的 JSON-line 风格，降低心智负担
4. 单个 CLI 挂掉不影响其他 session

### 非目标（明确写死，避免后续争论）

- **不做鉴权代理**：每家 CLI 自己管登录（`~/.claude/credentials`、`~/.codex/` 等），adapter 只 spawn 子进程
- **不做 context 重建**：会话历史归 CLI 自己管，adapter 只缓存 session ID 和能力声明
- **不做 agent 间 hand-off**：切换 agent 等于新开会话，不迁移历史
- **音频不走本协议**：TTS/ASR 走 cloud_saas 或 adapter 自己的音频通道（见 `audio_pipeline.md` 待建）

## 2. 总体架构

```
┌──────────────┐       BLE/USB/WiFi          ┌──────────────────────────┐
│  bbclaw HW   │◄───── JSON-line 帧 ────────►│  bbclaw-adapter  (Go)    │
│  (ESP32-S3)  │                              │                          │
└──────────────┘                              │  ┌────────────────────┐  │
                                              │  │ Agent Router       │  │
                                              │  │ (session → driver) │  │
                                              │  └────────┬───────────┘  │
                                              │           │              │
                                              │   ┌───────┼────────┐     │
                                              │   ▼       ▼        ▼     │
                                              │ ┌─────┐ ┌─────┐ ┌─────┐  │
                                              │ │claud│ │codex│ │aider│  │
                                              │ │-code│ │     │ │     │  │
                                              │ │drv  │ │ drv │ │ drv │  │
                                              │ └──┬──┘ └──┬──┘ └──┬──┘  │
                                              └────┼───────┼───────┼─────┘
                                                   │       │       │
                                                   ▼       ▼       ▼
                                              ┌────────┐ ┌───┐ ┌──────┐
                                              │claude  │ │cdx│ │aider │
                                              │ -p ... │ │   │ │      │
                                              └────────┘ └───┘ └──────┘
                                               (subprocess per session)
```

adapter 内部三层：

- **Frame layer**：解析/编码设备帧，和 buddy 风格兼容
- **Router layer**：session ID ↔ driver 映射、订阅设备事件分发
- **Driver layer**：每家 CLI 一个实现，统一 `AgentDriver` 接口

## 3. AgentDriver 接口

Go 接口定义（放 `adapter/internal/agent/driver.go`）：

```go
package agent

type SessionID string
type ToolID string

type Driver interface {
    // Name 返回 driver 唯一名（"claude-code", "codex", ...），设备端据此切换
    Name() string

    // Capabilities 声明这个 driver 支持哪些能力
    Capabilities() Capabilities

    // Start 拉起一个新会话；resume 非空则尝试恢复
    Start(ctx context.Context, opts StartOpts) (SessionID, error)

    // Send 把用户消息塞进 session 的 stdin
    Send(sid SessionID, text string) error

    // Events 返回 session 的事件流，driver 关闭 channel 表示 session 结束
    Events(sid SessionID) <-chan Event

    // Approve 回复一个 tool 权限决定；不支持审批的 driver 返回 ErrUnsupported
    Approve(sid SessionID, tid ToolID, decision Decision) error

    // Stop 主动结束 session（SIGTERM → 超时后 SIGKILL）
    Stop(sid SessionID) error
}

type StartOpts struct {
    ResumeID string // 空表示新会话
    Cwd      string // CLI 工作目录
    Env      map[string]string
}

type Decision string
const (
    DecisionOnce Decision = "once"
    DecisionDeny Decision = "deny"
)
```

### 生命周期要求

- `Start` 返回后 `Events(sid)` 必须立刻可用
- Driver 挂掉要发 `EventStatus{State: "offline", Reason: "..."}` 再关 channel
- `Stop` 必须幂等
- adapter 退出时调用所有活跃 session 的 `Stop`，禁止留僵尸进程

## 4. Capabilities 声明

每个 driver 必须声明能力，设备端 UX 据此调整：

```go
type Capabilities struct {
    ToolApproval   bool   // 支持 permission prompt 回环？
    Resume         bool   // 支持恢复历史会话？
    Streaming      bool   // 支持边生成边推 text 片段？
    MaxInputBytes  int    // 单条 user_msg 最大字节数
    ConcurrentSess int    // 允许几个并发 session
}
```

| CLI | ToolApproval | Resume | Streaming | 备注 |
|---|---|---|---|---|
| Claude Code | ✅ | ✅ | ✅ | `claude -p --output-format stream-json --verbose` |
| Codex | ✅（模型不同） | ✅ | ✅ | approval 模型更粗粒度 |
| Aider | ❌（默认自动执行） | ✅ | ✅ | 不暴露审批点 |
| Gemini CLI | 待调研 | 待调研 | ✅ | |
| Ollama/LM Studio | ❌ | ❌（无 agent 概念） | ✅ | 纯 chat，无 tool use |

设备端拿到 capabilities 后：

- `ToolApproval=false` 时隐藏"审批"菜单
- `Resume=false` 时切走再切回就是新会话
- 根据 `MaxInputBytes` 截断或分段用户输入

## 5. 统一事件 Schema

所有 driver 吐出的 `Event` 都映射到这几种：

```go
type Event struct {
    Type    EventType
    Seq     uint64    // driver 内单调递增
    Text    string    // text / status / error 复用
    Tool    *ToolCall // 仅 tool_call
    Tokens  *Tokens   // 仅 token 统计
    Status  *Status   // 仅 status
    TurnEnd bool      // 本次 turn 是否结束
}

type EventType string
const (
    EvText     EventType = "text"       // 助手文本片段
    EvToolCall EventType = "tool_call"  // 权限请求（Capabilities.ToolApproval 时）
    EvStatus   EventType = "status"     // running/waiting/idle/offline
    EvTokens   EventType = "tokens"     // 用量统计
    EvError    EventType = "error"      // driver 层错误
    EvTurnEnd  EventType = "turn_end"   // 一轮结束
)

type ToolCall struct {
    ID   ToolID
    Tool string  // "Bash" / "Edit" / ...
    Hint string  // 简短描述（给小屏幕用）
}
```

## 6. 设备 ↔ adapter 帧扩展

沿用 buddy 的 "每行一个 JSON 对象，`\n` 分隔" 约定。在 buddy 已有帧基础上**新增**：

### 设备 → adapter

| 帧 | 用途 |
|---|---|
| `{"cmd":"user_msg","sid":"...","text":"..."}` | 把用户输入塞给当前 session |
| `{"cmd":"session_new","driver":"claude-code","cwd":"..."}` | 新开会话，返回 sid |
| `{"cmd":"session_resume","driver":"claude-code","resume_id":"..."}` | 恢复历史会话 |
| `{"cmd":"session_stop","sid":"..."}` | 结束会话 |
| `{"cmd":"switch_agent","sid":"..."}` | 切到 sid 作为当前活跃会话 |
| `{"cmd":"list_drivers"}` | 查可用 driver 列表 |
| `{"cmd":"permission","sid":"...","id":"...","decision":"once"}` | 审批（沿用 buddy 语义，加 sid） |

### adapter → 设备

| 帧 | 用途 |
|---|---|
| `{"evt":"text","sid":"...","text":"...","turn_end":false}` | 助手文本片段 |
| `{"evt":"tool_call","sid":"...","id":"...","tool":"Bash","hint":"..."}` | 权限请求 |
| `{"evt":"status","sid":"...","state":"running"}` | 状态变化 |
| `{"evt":"tokens","sid":"...","in":123,"out":456}` | 用量 |
| `{"evt":"session_started","sid":"...","driver":"...","caps":{...}}` | 会话建立，附带 capabilities |
| `{"evt":"session_ended","sid":"...","reason":"..."}` | 会话结束 |
| `{"evt":"drivers","list":[{"name":"claude-code","caps":{...}},...]}` | 回 `list_drivers` |

### Ack 约定

所有 `cmd` 帧必须收到 `{"ack":"<cmd>","ok":true/false,"error":"...","data":{...}}`。

## 7. 错误处理与降级

| 场景 | 行为 |
|---|---|
| CLI 找不到（`exec: "claude" not found`） | `session_new` 回 `ok:false, error:"driver_unavailable"`；`list_drivers` 里标 `available:false` |
| CLI 鉴权失败 | driver 探测到后发 `EvError{Text:"auth_required"}` + `EvStatus{State:"offline"}` |
| CLI 输出速率过高（> 50 event/s） | adapter 层做**节流聚合**：同 sid 的 `text` 事件 100ms 内合并成一帧再推设备 |
| CLI 崩溃 | driver 发 `session_ended{reason:"crashed"}`，router 清理 sid；设备端提示用户 |
| adapter 掉线 | 设备端切到 `sleep` 态，显示"桌面未连接"；重连后自动 resume 活跃 session（若 `Resume=true`） |
| 设备帧解析失败 | adapter 丢弃该帧并日志记录，不向设备回错（避免反向循环） |

## 8. 设备端 UX 约束

- **agent 切换**：长按 A → 菜单 → `Agent: Claude Code ▸` → 子菜单列出 `list_drivers` 结果
- **当前 agent 持久化**到 NVS，下次启动默认用同一个
- **多 session 并发**：最小可行先做**单活跃 session**，capabilities 里 `ConcurrentSess` 先写 1
- **输入方式**：按键/转盘/语音（ASR 路径独立，见音频设计）→ 最终变成一个 `user_msg` 帧

## 9. 落地路径

> **状态**：Phase 1 → 5 全部 ✅ 落地，**v0.4.0 于 2026-04-27 发布**。下表是事后回顾，
> 子阶段编号沿用历史顺序（1 → 1.5 → 2 → 3 → 3.5 → 4 → 5），便于回查 commit。

| Phase | 内容 | 状态 |
|---|---|---|
| 1 | adapter 加 `driver/claude_code` + `AgentDriver` 接口 + `POST /v1/agent/message` NDJSON 端点 | ✅ 完成（2026-04-25） |
| 1.5 | 多轮 session 续接（`sessionId` 复用 + sweeper）· [ADR-002](decisions/ADR-002-multi-turn-session-lifecycle.md) | ✅ 完成（2026-04-25） |
| 2 | playground / web 调试入口（HTML 直连 NDJSON、tool_use 显示） | ✅ 完成（2026-04-26） |
| 3 | `Router` 抽象 + 第二个 driver（Ollama）+ `GET /v1/agent/drivers` · [ADR-003](decisions/ADR-003-router-and-multi-driver.md) | ✅ 完成（2026-04-26） |
| 3.5 | adapter env 收敛（删多余 env，只留 3 个必需变量） | ✅ 完成（2026-04-26） |
| 4 | 固件接入 Agent Bus（设备端聊天 / driver 切换 / NVS 持久化 / 语音桥 / TTS 流 / openclaw driver / cloud 反代）· 详细子阶段见 [firmware_agent_integration.md](firmware_agent_integration.md) §7 | ✅ 完成（2026-04-27，v0.4.0）|
| 4.0 | `bb_agent_client` 模块（HTTP NDJSON 流式 + cJSON 解析） | ✅ commit `0535218` |
| 4.1 | LVGL Agent Chat 屏幕 + 主题接口 + `text-only` 默认主题 | ✅ commit `6a18b71` |
| 4.2 | Settings 子菜单（driver / theme / TTS toggle） | ✅ commit `43097f1`（被 4.7 取代为独立 overlay） |
| 4.2.5 | 异步 driver 列表拉取 + Loading 占位 + cancel | ✅ commit `970e8bf` |
| 4.5 | PTT → ASR → Agent Bus 语音桥 | ✅ commit `080e43e` |
| 4.5.1 | TTS reply toggle（end-of-turn 一次性合成） | ✅ commit `a3a17a1` |
| 4.5.2 | 句级流式 TTS + cancel-and-replace + UTF-8 截断 | ✅ commit `fb3df9c` |
| 4.6 | `buddy-ascii` 七态主题（移植 claude-desktop-buddy 角色） | ✅ commit `2e47346` |
| 4.7 | 独立 Settings overlay（脱离 picker，全屏覆盖式）· 见 ADR-007 | ✅ commit `763fd0a` |
| 4.8 | cloud `/v1/agent/*` 反向代理 + 固件 deviceId 透传（cloud_saas 设备解锁 Agent Chat） | ✅ commits `efc8588`, `877f206`（bbclaw-reference）/ `3539137`（firmware） |
| 4.8.x | Chat 作为 standby + idle 自动退出 + LISTENING/SPEAKING 九态 state flow · 见 ADR-008、ADR-009 | ✅ commits `57da618`, `bf7f228` |
| 4.9 | openclaw 作为 AgentDriver（adapter 内置） · 见 [ADR-005](decisions/ADR-005-openclaw-as-driver.md) | ✅ commit `7356eda`（bbclaw-reference） |
| 5 | Flipper 6-button 完整 nav events Option B（UP/DOWN/LEFT/RIGHT/OK/BACK 全语义） · 见 ADR-006 | ✅ commit `99dd2ec` |
| 4.6.x | `buddy-anim` 主题（LVGL anim 驱动 9 态 ASCII 表情：呼吸 / 弹跳 / 抖动 / 心跳 / 闪烁） | ✅ post-v0.4.0 |
| 4.10 | `aider` 作为第 5 个 AgentDriver（plain-text stdout，`--chat-history-file` 续接） | ✅ post-v0.4.0 |
| (后续) | Codex / Gemini driver、buddy 像素图主题、tool_use 真审批闭环 | 按需加 |

### Phase 1 验收记录

端到端联调 2026-04-25：独立终端 `curl -N -d '{"text":"say hi in 5 words"}' http://localhost:18080/v1/agent/message` →

```
{"seq":1,"text":"Hi there, how are you?","type":"text"}
{"in":131,"out":37,"seq":2,"type":"tokens"}
{"seq":3,"type":"turn_end"}
```

Phase 1 实际实现略超原计划（直接把 `AgentDriver` 接口抽了，不是先硬编码再重构），因为增量成本低且避免二次改动。原 Phase 2 "抽接口" 合并入 Phase 1。

## 10. 开放问题

- **session 生命周期跨 adapter 重启**？CLI 进程能留命吗？（Claude Code 的 `--resume` 能部分解决）
- **agent 间协作**？（先排除，以后单独写 ADR）
- **设备端历史上下文缓存**？当前假设全部在 CLI 侧，但离线显示需要设备缓存最近 N 条 — 待讨论
