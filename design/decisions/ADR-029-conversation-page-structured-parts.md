# ADR-029: Adapter 对话页 v2 — 结构化 Turn-Part 持久化 + 思考流 / 派发子线程可视化

- 状态：草拟（2026-06-13），待评审。
- 关联：ADR-028（Conversation Core v2，Turn 模型 / `turn.state` thinking 子阶段 / `Interrupt` 语义——本文复用其 thinking 信号源与 dispatch-是独立进程的事实）、ADR-021（对话式编排管家）、ADR-021-firmware-ui（dispatch ring buffer §1.2/§1.4，本文扩展其持久化）、ADR-013（会话历史回放）、ADR-024（多 driver 管家生态——thinking 捕获的 per-driver 能力差异）、`design/UI_DESIGN_LANGUAGE.md` §1 原则 5（青=bbclaw 全线，对话页一律用青）。
- 范围：**adapter Web 管理面板对话页**（`adapter/web` Vue3，`Conversation.vue`），即 `/admin/conversations`。不改设备固件屏幕（设备侧仍按 ADR-028 走 `turn.state` 粗粒度子标签）。
- 参考原型：`design/prototypes/conversation-redesign.html`（思考折叠 + 内联/异步派发卡片 + 子线程下钻）。

## 1. 问题：对话页只能看到"压扁的最终文本"

现状 adapter 自带 Vue 管理面板（`adapter/web`），对话页 `Conversation.vue` 把一轮回合渲染成"用户气泡 + 助手纯文本回复"，3s 轮询拉增量（`Conversation.vue:318`）。它丢失了管家执行过程中最有价值的两类信息：

### 1.1 思考过程被丢弃（不是拿不到，是没解析）

claude-code 驱动逐行解析 `stream-json`，content-block 分发只处理 `text` / `tool_use` 两类（`adapter/internal/agent/claudecode/driver.go:600-644`），**没有 `thinking` 分支**，thinking 块静默跳过。

实测（claude 2.1.177，2026-06-13）确认 thinking **可达**但需配置：

| 启动方式 | 是否吐 thinking |
|---|---|
| 现状 `claude -p … --output-format stream-json --verbose`（`pool.go:255` / `driver.go:243`） | ❌ |
| `MAX_THINKING_TOKENS` env | ❌ |
| `--settings '{"alwaysThinkingEnabled":true}'` + 够烧脑的提示词 | ✅ 最终 `assistant` 信封带完整 `thinking` 块 |
| 上者再加 `--include-partial-messages` | ✅ 额外得到 `thinking_delta` / `signature_delta` 流式增量 |

结论：折叠展示历史只需开 `alwaysThinkingEnabled`，driver 加一个 `case "thinking"`；实时打字机才需 `--include-partial-messages`。

### 1.2 派发子任务只剩一行状态、内容全丢

派出去的子 `claude -p` worker 是**完整 session**：runner 消费它**自己的 event 流**后只保留 ≤8KB 最终文本交还管家，thinking / tool / 嵌套派发全部丢弃（`adapter/internal/butlermcp/runner_claude.go:14-30`）。前端只拿到 `EvDispatchStatus`（`agent/driver.go:54-70`）的扁平状态（taskId/cwd/title/status/elapsedMs），存进 50 条 ring（`butler/dispatch_ring.go`），渲染成一个平铺侧栏，**无法下钻看子 agent 到底做了什么、怎么想的**。

### 1.3 派发是异步的，不能照搬阻塞式 UI

`mcp__bbclaw__dispatch(wait_seconds=25)`：25s 内跑完则内联返回 `{status:"done"}`，超时**降级 async** 返回 `{status:"running", taskId}`，worker 在后台 ctx 继续、管家靠 `task_status`/`task_result` 轮询回收（`butlermcp/server.go`，ADR-021 §2）。因此派发卡片是一个**跨轮次的异步 job**——终态可能落在后续某轮，UI 必须支持原位异步回填，不能当成同步子步骤。

## 2. 决策

把对话页的数据模型从"扁平文本"升级为**带类型的有序 Turn-Part 序列**，并补齐 thinking 捕获与派发子线程链接。

### 2.1 Turn-Part 持久化模型（核心）

一轮 assistant 回合持久化为有序 `Part` 列表，`Seq` 保证时序：

```
Part {
  Kind  : "thinking" | "text" | "tool" | "dispatch"
  Seq   : int
  Text  : string          // thinking 内容 / 回复文本 / tool hint
  Tool  : *ToolRef         // Kind=tool
  Disp  : *DispatchPart    // Kind=dispatch
  Ts    : string           // RFC3339
}
DispatchPart {
  TaskID, Cwd, Title : string
  Status             : "started" | "running" | "done" | "error"   // 异步生命周期
  ElapsedMs          : int64
  Error              : string
  ChildSessionID     : string   // ★新增：指向 worker 自己的 session，供下钻
}
```

- **向后兼容（硬约束）**：现有扁平 `Message{Role,Content,Seq,Timestamp}`（`driver.go:174`）**保留**，其 `Content` 退化为所有 `text` part 的拼接投影。设备固件、`turn.state` 协议、旧 reader 不受影响；Part 日志是**增量旁路**，仅 admin 对话页消费。
- 新增 HTTP 端点 `GET /v1/admin/sessions/{id}/parts`（或在现有 messages 端点加 `?format=parts`），返回分段/分轮的 Part 流，沿用现有 seq 游标分页。

### 2.2 思考捕获

1. **启用**：claudecode driver 启动参数注入 `--settings '{"alwaysThinkingEnabled":true}'`（经 `Options.ExtraArgs`），由配置开关 `AGENT_THINKING=on|off` 控制（默认 on——admin 面板是 localhost-only）。**不默认加** `--include-partial-messages`。
2. **解析**：`parseStreamJSON` 的 `assistant` 信封 content-block switch 增加 `case "thinking"` → `s.emit(Event{Type: EvThinking, Text: c.Thinking})`。新增事件类型 `EvThinking`（`agent/driver.go` EventType 枚举）。
3. **持久化**：thinking 作为 `Kind:"thinking"` part 落库，带长度上限（建议 8KB/块，超出省略中段并标记）。
4. **与 ADR-028 统一**：同一 thinking 信号同时驱动设备侧 `turn.state{phase:"thinking"}`（粗标签）与 admin 侧 thinking part（全文）——一个源，两个粒度。
5. **per-driver 能力**：thinking 仅 claude-code 已验证可达；opencode driver 有 `reasoning` 事件（仅日志、未 emit，可后续接）；其余 driver 无。对话页对无 thinking 的 driver 直接不渲染思考块，不报错（ADR-024 能力矩阵的一项）。

### 2.3 派发子线程下钻

1. **保留子 session**：`DriverWorkerRunner` 当前消费完子事件流即弃。改为：worker 的 session 注册进会话存储（它本就是一个 claudecode session，有自己的 sid 与 Part 流），runner 在仍向管家返回 ≤8KB 摘要的同时，**把 worker sid 记入 `DispatchPart.ChildSessionID`**。
2. **下钻渲染**：对话页派发卡片展开时，按 `ChildSessionID` 懒加载子 session 的 Part 流，缩进一级递归渲染（子的 thinking / text / 再派发）。这是 bbclaw 独有、agent_room（其子任务仅 shell stdout/stderr）做不到的能力。
3. **边界**：递归下钻默认只展开一层；多级嵌套派发按需逐层点开，避免一次性拉爆。子 session 的 thinking 同样受 §2.2 开关与长度上限约束。

### 2.4 异步派发的 UI 生命周期

- 派发卡片三态描边 + chip：`started/running`=青呼吸点「派发中·async」、`done`=`var(--ok)`绿「✓ Ns」、`error`=`var(--err)`红。
- 内联完成（≤25s）：当轮原位 done。降级异步：先「派发中」挂起，`task_status` 轮询到终态后**原位回填**（青闪一拍沉淀，dot-matrix-ui 点亮节奏），并由管家在后续轮次补一条 text part 汇报。
- v1 沿用现有 3s 轮询即可覆盖异步回填；实时性升级（SSE / 复用 ADR-028 的 WS 事件流）为后续可选项，不阻塞本 ADR。

### 2.5 视觉

严格遵循 `design/UI_DESIGN_LANGUAGE.md` 点阵语言，accent = 青 `#2ec4a0`（§1 原则 5：bbclaw 全线用青，**不得借用云端黄**）。思考块 = 青色左缘折叠时间线（默认收起一行摘要）；派发卡片 = ghost 描边卡片 + 状态 chip；选中/新增走青左竖条与青闪节奏。落地参考 `design/prototypes/conversation-redesign.html`。

## 3. 实施计划（每阶段独立可验证）

| 阶段 | 内容 | 依赖 |
|---|---|---|
| M1 思考捕获 | driver 启用 `alwaysThinkingEnabled` + `case "thinking"` → `EvThinking`；配置开关 | 无（最小改动，先出"看得到思考"） |
| M2 Part 持久化 | Part / DispatchPart 模型 + 落库；扁平 Content 退化为投影；`/parts` 端点 | M1 |
| M3 前端渲染 | `Conversation.vue` 按 Part 类型渲染：思考折叠流 + 内联/异步派发卡片（点阵青） | M2 |
| M4 子线程下钻 | worker session 注册留存 + `ChildSessionID` 记录；卡片懒加载子 Part 流递归渲染 | M2、M3 |

M1+M3 即可交付"思考可见 + 派发内联"的可用版本；M2 让历史可回看；M4 完成下钻。

## 4. 验收 / 边界

| 项 | 现状 | 目标 |
|---|---|---|
| 历史里看到管家思考过程 | ❌ 丢弃 | ✅ 折叠时间线，可展开 |
| 派发记录位置 | 平铺侧栏 | 按时序内联进对话流 |
| 派发子任务内容 | 仅一行状态 | 可下钻看子 agent 完整 thinking+回复 |
| 异步派发终态回填 | 侧栏轮询刷新 | 卡片原位回填 + 管家汇报气泡 |
| 配色 | 通用 | 点阵青 #2ec4a0，与云端黄分层 |

**明确不做**：① 不改设备固件屏幕（设备侧维持 ADR-028 粗粒度子标签）；② v1 不上实时流（保留 3s 轮询）；③ thinking 持久化受配置开关控制，关掉则只走 §2.2 实时不落库；④ 多级派发下钻不自动全展开。

**可复用回 agent_room**：Part-Kind 模型 + childSessionID 下钻是通用设计，agent_room 的 delegate_exec 若也挂一个 child room/session 引用，即可复用同一套"子线程递归渲染"。
