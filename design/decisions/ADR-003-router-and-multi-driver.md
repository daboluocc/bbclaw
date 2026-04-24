# ADR-003: Router + 多 driver 路由策略

- **编号**: ADR-003
- **标题**: adapter 内 Router 抽象 + 会话绑定 driver + 内存归属修正
- **日期**: 2026-04-25
- **状态**: 已接受
- **相关文档**: [agent_bus.md](../agent_bus.md)、[ADR-001](ADR-001-adapter-as-agent-bus.md)、[ADR-002](ADR-002-multi-turn-session-lifecycle.md)

## 背景

Phase 1/1.5 只挂一个 driver（claude-code），`AgentDriver` 接口是否真的够用"无法验证"。Phase 3 的核心动作是**加第二个 driver 跑一遍**，把抽象走穿。选 Ollama 作为第二个 driver 是因为：

- 本地运行，无鉴权
- HTTP `/api/chat` 原生 NDJSON 流，和现有 Event schema 天然对齐
- **没有 `--resume` 机制**，会倒逼我们把 ADR-002 的"记忆归 CLI"原则说清楚

## 决策 1：Router 抽象形状

`internal/agent/router.go`：

```go
type DriverInfo struct {
    Name         string
    Capabilities Capabilities
}

type Router struct {
    // drivers: name → Driver，首次 Register 的成为 default
    // defaultName: 第一个注册的 driver 名
}

func NewRouter() *Router
func (r *Router) Register(d Driver, log *obs.Logger)
func (r *Router) Get(name string) (Driver, bool)
func (r *Router) Default() Driver
func (r *Router) List() []DriverInfo
```

关键点：

- **首次注册者为默认**（simple and deterministic，不搞 "default" 单独字段给人去配）
- **重复 Register 覆盖 + 警告**（不 panic，生产里希望热替换而不是崩掉）
- List 按名字字典序排序（稳定的设备 UX）

### 为什么不是 map 直接暴露

- 并发安全由 Router 自己管（`sync.RWMutex`），外部不碰锁
- 将来加 "disable driver at runtime" / "driver health check" 等行为时不破坏调用方

## 决策 2：Session 与 Driver 绑定

**一个 session 一旦 Start，就锁死到某个 driver 上，直到 Stop。**

- `sessionEntry` 加字段 `driverName string`
- 请求里可选 `driver`：
  - 无 sessionId：`driver` 指定走哪个；缺省走 default
  - 有 sessionId：忽略 `driver`（用存下来的 driverName），**除非**客户端显式传了不同的 `driver` → 返回 `400 SESSION_DRIVER_MISMATCH`
- Sweeper 淘汰 session 时按 `driverName` 去 Router 找对应 driver 调 Stop

**为什么锁死而不是允许切换**：

- 切换 driver 等于切换对话历史的存储介质，语义混乱（claude-code 在 `~/.claude/...`，Ollama 在 driver.session.messages）
- 设备端 UX 更简单：要换模型，**丢弃旧 sessionId 重开**
- 一致性错误比静默接受更好 debug

## 决策 3：内存归属原则 — 修正 ADR-002

ADR-002 说："所有'记忆'由 CLI 侧的 resume 机制承担"。这在 claude-code 成立，在 Ollama 不成立 —— Ollama 没有 resume，想要多轮就必须把消息历史存在**某个地方**。

**修正后的原则（以本 ADR 为准）**：

| Driver 类型 | 记忆归属 | 例子 |
|---|---|---|
| 有 CLI-side resume 机制 | CLI 自己管（adapter 零状态） | claude-code（`--resume`）、Codex |
| 无 resume | **driver.session 内存缓存 `messages[]`**，Send 时全量发 | Ollama、直连 LLM API 类 |
| 既无 resume 又无意义维护历史 | Capabilities.Resume=false，诚实声明 | `echo` 类 mock，单轮查询 driver |

**统一底线**：adapter 主进程（router/sessionRegistry）**永远不**持有对话内容，只持有 `sessionId → driverName + SessionID` 映射。对话内容要么在 CLI 磁盘，要么在 driver.session 内存 —— **不越层**。

Ollama driver 的消息历史长度上限 **50 turns**，超了丢最旧（FIFO）。这是"够用 + 不爆内存"的经验值，以后做成配置项也不迟。

## 决策 4：新增 `GET /v1/agent/drivers`

设备端需要知道有哪些 driver 可选 + 各自 capabilities，才能在屏幕上画切换菜单（Phase 4）。

响应：

```json
{
  "ok": true,
  "data": {
    "drivers": [
      {"name": "claude-code", "capabilities": {"toolApproval": false, "resume": true, "streaming": true, "maxInputBytes": 65536}},
      {"name": "ollama", "capabilities": {"toolApproval": false, "resume": true, "streaming": true, "maxInputBytes": 65536}}
    ]
  }
}
```

为什么放在 HTTP 而不是 WebSocket 推送：Phase 1 起的通道就是 HTTP NDJSON，driver list 是**偶尔查一次**的元数据，不需要推送语义。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 不做 Router，每个 driver 挂一个独立 HTTP path（`/v1/agent/claude-code/message`、`/v1/agent/ollama/message`） | URL 与配置耦合，设备端切换要改 URL；加新 driver 要改客户端 |
| Router 支持 driver 别名/多名 | 早期无意义复杂度 |
| session 允许中途切换 driver | 语义混乱（见决策 2） |
| 消息历史放 adapter 全局（不在 driver 里） | 违反"不越层"底线；driver 之间历史混用毫无意义 |

## 后果

### 正向

- `AgentDriver` 抽象被第二个实现验证，漏抽的地方现在暴露就能补，以后加 Codex/Aider/Gemini 成本线性
- 设备端 UX 解锁：Phase 4 的"agent 切换菜单"有了数据源
- 内存归属原则统一成文，不同 driver 的"记忆"去哪一眼看得清

### 负向

- Ollama 上下文随对话轮数线性增长 token 开销（无 prefix caching 优化）
  - 缓解：50-turn 上限 + 用户教育"换模型=新对话"
- Session ↔ Driver 硬绑定让"半路切模型对比"之类场景做不了
  - 真需要时：客户端自己维护 sessionId-A（claude）、sessionId-B（ollama），发两次 request

### 需后续决策的开放问题

- Driver health check / 运行时启停 → ADR-004
- 生产化 driver 鉴权/限流 → 随生产化阶段的 ADR 一起定
- Ollama `messages[]` cap 做成按 token 数截断（更精准）→ 真遇到长对话再做
