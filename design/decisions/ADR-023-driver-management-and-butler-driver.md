# ADR-023: 驱动管理 —— 通用驱动 / 独立 butler 驱动 / 环境检测接入页面

- **日期**: 2026-06-10
- **状态**: 已接受（v1 范围明确）
- **关联**: ADR-021（对话编排管家 —— butler 会话固定驱动的来源）、ADR-019（服务端驱动菜单协议 —— device 的 `set_driver`）、ADR-018（设备管家架构）、ADR-016（设备侧驱动/模型选择 —— active_driver/active_model 持久化）、ADR-011（adapter 开源迁移）

## 背景

Adapter 已经有完整的**通用驱动切换**后端：`driverstate` 持久化 `active_driver` + `PUT /v1/agent/active_driver`（同时 `router.SetDefault` 镜像），切换对**下一条不指定 driver 的消息**立刻生效——claudecode 驱动每轮新起 `claude -p … --resume`，是无状态重拉，无需重启进程。device 也能通过服务端菜单（ADR-019 的 `set_driver`）切。

但要让「用户在 adapter 页面改驱动、默认检测环境、默认 claude、切了立刻生效」真正可用，有三处缺口：

1. **Web 页面只读** —— admin SPA 的 StatusBar 只列出已注册驱动，没有启用/切换 UI；`api.ts` 连 `setActiveDriver` 都没有。
2. **环境检测没接进页面** —— `internal/detect` 能 LookPath 检测 claude/opencode/aider/ollama/openclaw，但只被 `doctor` CLI 和 envfile 生成用，没接 HTTP。更糟的是 `main.go` 里 claude-code / opencode 的 `autoEnable` 写死 `return true`：**没装也注册**，跑起来调用才失败。
3. **butler 驱动硬编码 claude-code** —— 真机默认走 butler 模式（ADR-021），每个 device turn 都 `EnsureButler(deviceID, butler.ButlerDriver="claude-code", …)`。因此在真机上切 `active_driver` 对管家**完全不生效**——`active_driver` 管的是 playground / Agent Bus 非-butler 会话，跟设备管家用哪个 CLI 是两回事。

把这两者混成一个设置很危险：butler 依赖 `--append-system-prompt`（注入 persona）和 `--mcp-config`（dispatch 工具 `mcp__bbclaw__dispatch`），**目前只有 claudecode 驱动落实了这两项**。如果让 butler「跟随 active_driver」，用户在页面把通用驱动切到 ollama / codex，管家的 persona 和派活能力会直接哑掉，且无明显报错。

## 决策

### 1. 两个独立的驱动设置

| | 通用驱动 `active_driver` | 设备管家驱动 `butler_driver`（本 ADR 新增） |
|---|---|---|
| 谁用 | 网页 playground / Agent Bus 非-butler 多会话 | 真机语音/文本 → 管家 logical session |
| 候选 | 任意已注册驱动 | **仅 butler-capable**（见 §2） |
| 默认 | 第一个注册的驱动（注册顺序里 claude-code 居首） | claude-code |
| 持久化 | `driver_state.json` 的 `active_driver`（现状） | `driver_state.json` 新增 `butler_driver` |
| 立刻生效 | 切后下一条不指定 driver 的消息（`router.SetDefault` 镜像） | 切后下一轮 device turn（`EnsureButler` 每轮调用，按 `deviceID+driver` 建键，自然起新管家会话） |

`butler_driver` **不跟随** `active_driver`，二者各切各的。切 `butler_driver` 会换一个新的 butler logical session（旧的被 sweeper 回收），等于管家丢掉上一段对话连续性——可接受，与 ADR-016 的 active_model 切换语义一致。

### 2. butler-capable 由驱动自报

新增 `agent.Capabilities.Butler bool`。初版**只有 claudecode 设 `Butler: true`**；opencode / aider / ollama / openclaw / codex 均 false。将来某驱动落实了 `--append-system-prompt` + MCP-config 等价能力，翻开它的标志即可，无需改 butler 引擎。

`PUT /v1/agent/butler_driver` 校验目标**已注册且 `Capabilities().Butler == true`**，否则 `400 NOT_BUTLER_CAPABLE`。

### 3. 真 LookPath 门控 + 环境检测接入页面

- `main.go` 把 claude-code / opencode 的 `autoEnable` 改成真 `exec.LookPath`，各配一个 `forceEnv`（`AGENT_CLAUDE_CODE_FORCE` / `AGENT_OPENCODE_FORCE`）逃生门，与 ollama/openclaw/aider 既有约定一致。没装的驱动不再注册。
- 新增 `GET /v1/agent/environment`：调 `detect.DetectAll`，返回每个驱动 `{installed, reason}`。检测要用各驱动**自己的逻辑**（openclaw=配置文件+token，ollama=TCP dial，其余=LookPath），不能统一 LookPath。
- `GET /v1/agent/drivers` 每行加 `installed` / `butler_capable`，顶层加 `butler_driver`。

### 4. 新增 codex 驱动

OpenAI codex CLI，非交互 `codex exec --json`（JSONL 事件流，`thread.started.thread_id` 即 resume id，`codex exec resume <id>` 续话）。建模仿 opencode 驱动。codex 不是 butler-capable。

## 跨组件约束（务必同步）

`internal/homeadapter`（云端 relay）是一套**平行实现**，不是简单透传。它有自己的：
- drivers reply（`agent_proxy.go handleAgentDriversRequest`）—— 新字段 `installed/butler_capable/butler_driver` 必须在这里同步加，否则 LAN 直连和云/固件路径漂移。
- `agent.active_driver.set` setter —— 新增并列的 `agent.butler_driver.set` kind + handler + dispatch。
- **第二个 butler `EnsureButler` 调用点**（`adapter.go`）—— `resolveButlerDriver` 必须在 LAN（`httpapi/agent.go`）和云（`homeadapter/adapter.go`）**两处**都接上，否则 butler 驱动设置只在 LAN 生效。

## 范围外（v1 已知限制）

- **device 菜单维持只管 active_driver**。butler 驱动是「管家用哪个 CLI」的 adapter 级配置，不适合放小屏让终端用户切；后续若要，再给 ADR-019 的封闭 `Action` 集加 `set_butler_driver`。
- **驱动切换不走 WS 广播**。admin 页靠 refetch，多客户端同时开时不保证即时一致。
- **`agent.environment` 不做云 relay kind**。本期检测页面是 localhost 直连；固件若也要显示 installed 再加。
