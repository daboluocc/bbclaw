# ADR-002: 多轮会话生命周期（Agent Bus）

- **编号**: ADR-002
- **标题**: adapter 侧 session 注册表 + 客户端传 sessionId 复用（配 CLI 自己的 --resume）
- **日期**: 2026-04-25
- **状态**: 已接受
- **相关文档**: [agent_bus.md](../agent_bus.md) §8、[ADR-001](ADR-001-adapter-as-agent-bus.md)

## 背景

Phase 1 落地后，`POST /v1/agent/message` 是**一次性**的：每次请求 `Start → Send → Stop`，turn_end 后立刻销毁 driver session。在真实设备 UX 下这等于"用户每按一下 PTT，Claude 就忘了前面说过什么"，连续对话不可能。

claude-code driver 已经具备技术上的多轮能力 —— 它从 stream-json 的 `system.init` 事件里抓到 `cli_session_id` 存进 `session.resumeID`，后续 `Send` 会自动带 `--resume <id>`。**瓶颈在 HTTP 层**，不在 driver。

同时还发现一个隐含 bug：`Driver.Start()` 当前接收的 ctx 来自 `r.Context()`，这是**每请求上下文**，响应一返回就 cancel。跨请求复用 session 时第二次 Send 会因 ctx 已死直接失败。

## 决策

**adapter 侧维护一张 session 注册表，客户端在请求里带 `sessionId` 标识会话；底层真正的"记忆"由各 CLI 自己的 resume/session-id 机制承担。**

### 具体形状

- 请求体新增可选 `sessionId` 字段；缺省 = 新开会话
- 首帧恒为 `{"type":"session","sessionId":"...","isNew":true|false}`
- 已命中的 session：仅调 `Send`，**不** `Start`、**不** 在 turn_end 时 `Stop`
- 未命中：`Start` → 注册 → 返回新 id
- 后台 sweeper 按 5 min 周期扫描，闲置 >30 min 的 session 调 `Stop` 并出表
- driver 的 rootCtx 改为**服务端生命周期**（用 `s.agentCtx`），不再绑定请求

### Session 状态归属分层

| 层 | 存什么 | 谁管 |
|---|---|---|
| 客户端（设备 / web） | `sessionId` 字符串 | 自己持久化（NVS / localStorage），重连时回传 |
| adapter session 注册表 | `sessionId → driver.SessionID + lastUsed` | adapter 进程内存，TTL 失效 |
| driver.session 对象 | `resumeID`（CLI 的 session id） + events channel | Go 对象，Start/Stop 生命周期 |
| CLI 自己 | 对话历史、context、auth | 各家自己（`~/.claude/...` 等） |

**关键原则**：**adapter 不重建对话历史**。所有"记忆"由 CLI 侧的 resume 机制承担，adapter 只做"把同一个客户端路由到同一个 CLI session"的映射。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 无状态重同步（客户端每次带完整历史） | 设备端存储有限；token 经济学差（每轮重新 prefill 所有历史）；手机 claude-code `--continue` 的 prefix caching 利用不到 |
| 直接透传 CLI 的原生 session id 给客户端 | 暴露实现细节；换 driver 就崩；Aider / Ollama 等没有统一 session 概念 |
| adapter 持久化 session 到磁盘 | 复杂度上去了；重启语义不清；CLI 自己已有磁盘持久化（`~/.claude/projects/...`），重复 |
| 服务端 WebSocket 长连接替代 HTTP | 设备端 BLE 网关协议未定，保持 HTTP+NDJSON 和 Phase 1 一致；未来需要再升级 |

## 后果

### 正向

- 真正可用的连续对话 —— 这是设备端 UX 的前置依赖
- adapter 保持轻量：**无**对话历史、**无**磁盘状态，重启即清空，语义简单
- CLI 层切换（换 driver）时客户端只需丢掉旧 `sessionId` 重开，不破坏抽象
- 和 Phase 2 的 Router 天然兼容：注册表以 `sessionId` 为 key，driver 是值的一部分

### 负向

- **adapter 重启 = 所有会话丢失**。用户感受：按键后设备提示"会话已过期，新开对话"
  - 缓解：claude-code 的 CLI 级 `--resume` 不依赖 adapter，理论上客户端可以存 driver 返回的 cli-session-id 做"跨重启恢复"；但这是 ADR-003 的事，Phase 1.5 不做
- **TTL 是硬编码 30 min**，设备休眠久了会话会被扫掉
  - 缓解：Phase 2 如果需要，从环境变量读；现在没必要
- **无并发限制**：理论上恶意客户端可以无限 Start 积累 session
  - 缓解：Phase 2 加最大 session 数 + LRU 淘汰；生产环境加鉴权限流

### 需后续决策的开放问题

- 跨 adapter 重启的 session 恢复（把 cli-session-id 暴露给客户端？）→ ADR-003（TBD）
- 单客户端并发 session 数限制 → 和 Phase 2 Router 一起定
- sessionId 是否需要鉴权（防跨客户端窃取）→ 生产化阶段的 ADR
