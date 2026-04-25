# ADR-005: openclaw 接入 AgentDriver — 重新评估 ADR-001

- **编号**: ADR-005
- **标题**: openclaw 包成 `agent.Driver` 暴露在 Router，**不替换**现有 PTT/voice 路径
- **日期**: 2026-04-26
- **状态**: 已接受
- **相关文档**: [agent_bus.md](../agent_bus.md)、[ADR-001](ADR-001-adapter-as-agent-bus.md)、[ADR-003](ADR-003-router-and-multi-driver.md)

## 背景

[ADR-001](ADR-001-adapter-as-agent-bus.md) 选定 adapter 作为 Agent 总线时，**显式排除**了把 openclaw 也接入 `AgentDriver`：

> openclaw 是线上跑着的文字通路... AgentDriver 抽象才 2 个实现，**还没证明能容纳 openclaw 的复杂度**

那个保守判断在 Phase 1–3 阶段是对的。**Phase 1.0 → 4.2 真机验证后，前提变了**：
- claude-code driver（CLI 子进程 + `--resume`）
- ollama driver（HTTP 流 + 进程内 messages 历史）
- 抽象在两种**完全不同的 continuity 机制**下都站住了

设备端在 Phase 4.2 已通过 `GET /v1/agent/drivers` 动态拉 driver 列表 + 在菜单里切换。如果 openclaw 不挂进 Router，**用户在设备上看不见它**，cloud_saas 的核心通路成了 Agent Chat 看不见的"暗物质"。

## 决策

**实现 `internal/agent/openclawdriver/driver.go`，包一层 `*openclaw.Client`，注册到 Router 名为 "openclaw"**。

**不做的事**（明确）：
- 不替换 `internal/openclaw/Client` 本身
- 不动 PTT/voice 路径（`handleStream*` → `pipeline.Sink` → openclaw client）—— 该路径独立运行
- 不强制 home_adapter / cloud_relay 经过 driver

也就是说，openclaw 在 adapter 里**同时作为两种角色存在**：
1. 老路径：PTT 音频 → ASR → openclaw client → cloud LLM → TTS（通过 `buildSink`）
2. 新路径：设备 Agent Chat 文本 → openclaw driver → openclaw client → cloud LLM（通过 Router）

底层 client 共用，上层语义解耦。

## 协议映射

| openclaw 流事件 | 译为 Agent Bus 事件 | 备注 |
|---|---|---|
| `reply.delta` (text) | `EvText{Text}` | 直接转 |
| `tool_call` | `EvToolCall{Tool, Hint=""}` | openclaw 当前不暴露结构化 input；hint 留空 |
| `thinking` | **丢弃** | 不发明新事件类型；用户体验上 thinking 文本对设备屏幕是噪声 |
| stream 完成 | `EvTurnEnd` | |
| stream 错误 | `EvError` + `EvTurnEnd` | |

## Capabilities

```go
ToolApproval  = false  // openclaw 没有 permission.decision 回环；声明 true 等于撒谎
Resume        = true   // 通过 session_key（见下）
Streaming     = true   // SendVoiceTranscriptStream 原生支持 reply.delta
MaxInputBytes = 64 * 1024
```

## Session 模型 — 核心抽象点

openclaw 的多轮 continuity **不在 CLI 进程层**（不像 claude-code 的 `--resume`），而**在 gateway 服务端**通过 `session_key` 关联对话历史。

`agent.SessionID` 和 `openclaw session_key` **是两个独立标识符**：

| 标识符 | 谁管 | 生命期 |
|---|---|---|
| `agent.SessionID` ("oc-\<uuid>") | adapter Router 内部 | 进程生命期内 |
| `openclaw session_key` ("oc-\<uuid>") | gateway 那边的对话历史索引 | 跨 adapter 重启可恢复 |

**两者值上相同（都用 uuid），但用途分离**：
- driver `Send` 时把 `agent.SessionID` 翻译成 openclaw `SessionKey`（gateway 用它索引历史）
- `StartOpts.ResumeID` 非空 → 复用为 session_key（"我接着之前那个对话"）
- `StreamID` 是 per-turn 的（"oc-\<sid>-\<turn>"），让 gateway 区分一轮内的事件

## 元洞察 — 写 ADR 时学到的

ADR-001 担心 driver 接口"包不下" openclaw 的服务端架构。**两个 driver 落地后这个担忧消解了**：

> claudecode 通过 CLI 输出的 resume_id 实现 continuity；ollama 通过进程内 messages[] 实现；openclaw 通过 gateway 的 session_key 实现。三种机制在 driver 接口层都只是**driver 在 Send 时往返的不透明 token**，`StartOpts.ResumeID` 是统一抽象。

**结论**：`AgentDriver` 在"子进程 CLI"和"远端 WS 服务端 agent"两种形态间**通用、无张力**。继续用同样的接口加 Codex / Aider（Phase 5）。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 不接入 openclaw（保持 ADR-001 决策） | 设备菜单选不到 openclaw → cloud_saas 用户在设备上无法走核心通路 |
| 把 openclaw client 重写成符合 driver 习惯的接口 | 风险大、收益虚；wrapper 模式能完成 99% 价值 |
| 用一个新进程把 openclaw 协议也代理成像 claude-code 那样的 stdin/stdout CLI | 多一层桥；多 hop 多故障点 |

## 后果

### 正向

- 设备菜单（Phase 4.2.5）真正能切 claude-code / openclaw / ollama 三条路
- 抽象在三种不同 continuity 机制下被验证
- PTT 老路径**零影响**（client 共享，agent driver wrapper 只在新通路被触发）

### 负向

- 同一个 openclaw client 实例被两条路径并发使用（Router driver session + buildSink）→ 需要 client 内部并发安全；现有 client 是 stateless 的所以 OK，但要意识到这点
- gateway 那边可能看到两种 source 的请求（"bbclaw.adapter" vs "bbclaw.adapter.agent"）—— 留个 source 字段区分，gateway 侧统计需要
- Phase 4.8（cloud `/v1/agent/*` 反向代理）依然要做，因为云端没有 adapter，cloud_saas 设备到 Agent Chat 的 last mile 还缺
