# ADR-031: OpenCode 作为 canonical 后端 —— 收敛 driver 动物园，serve + SDK 取代 scrape-CLI

- **日期**: 2026-06-17
- **状态**: 已接受（spike 通过；v1 driver 已落地，开关后默认；butler/审批 UX 为 fast-follow）
- **重评**: ADR-024 的「N 个自包含 driver 生态」前提 —— 把「每家 CLI 各写一个 scrape driver」收敛为「**单一 canonical 后端 = OpenCode**，由它在内部适配 75+ provider」。ADR-024 的管家编排层（adapter 持唯一 persona + 基础知识、投影进原生文件、worker 与管家同源、记忆随激活后端）**保留**，只是「激活后端」从「claude/codex/opencode 任选其一各自适配」收敛为「OpenCode 一家，模型在它内部切」。
- **关联**: ADR-001（adapter 作为 Agent 总线）、ADR-003（Router + 多 driver）、ADR-005（openclaw-as-driver）、ADR-024（多-driver 自包含管家生态）、ADR-029（结构化 parts）、ADR-016（driver/model 选择）

## 背景

### 两层「适配」被混在一起

adapter 里有两件价值完全不同的事，过去都叫「适配」：

1. **设备网关层** —— WebSocket 设备协议、管家 persona、小屏 tool approval、transcript 回放、cloud relay。这是产品**唯一的护城河**：「一个能用语音跑编码 agent 的实体盒子」，别人 `brew install` 得不到。**这层永远是我们自己的，不外包。**
2. **每个 CLI 一个 scrape driver** —— `claudecode` / `codex` / `ollama` / `aider` / `openclaw` / `opencode`，各自逆向其 CLI 的 NDJSON/stream-json 输出。这是**真正吃力不讨好的跑步机**：维护 6 套、每套追一个上游、上游一升级就可能静默错位。

低收益的是 ②，不是 ①。本 ADR 只动 ②。

### 现状里 opencode driver 反而最弱

[internal/agent/opencode/driver.go](../../adapter/internal/agent/opencode/driver.go) 现在是「每轮 spawn `opencode run --format json`、scrape NDJSON」。它在我们自己体系里能力最差：

- `ToolApproval: false`（serve 不出审批）
- 无 `SessionLister` / `MessageLoader` / `PartLoader`（admin 页历史回放全缺）
- 无 `Interrupter`（不能 barge-in）
- `SystemPrompt` 靠 `OPENCODE_CONFIG_CONTENT` 临时文件 hack 注入
- model 列表靠手维护的 `models.go`

而 OpenCode 本身（1.15.1 实测）早就有一套**带版本的 server**（`opencode serve`，OpenAPI 3.1）+ 官方 Go SDK（`github.com/sst/opencode-sdk-go`），上面这些它全是一等公民。我们一直在 scrape 它的 CLI，却没用它的 server。

### 关键认知：「换 OpenCode」≠「放弃 Claude」

OpenCode 通过 Vercel AI SDK + models.dev 适配 **75+ provider**，**Anthropic 也是其中一家**。收敛到 OpenCode 丢掉的是 **Claude Code 这个 CLI 外壳**，不是 **Claude 这个模型**——Opus/Sonnet 仍能通过 OpenCode 的 provider 跑，MCP、subagent 也都在。

## 决策

**把 OpenCode 定为 canonical 后端：adapter 通过 `opencode serve`（OpenAPI/SDK）驱动它，由 OpenCode 在内部承担「适配 N 家模型 provider」，我们只维护对接它**一个稳定 API**的 driver。**

driver 从「每轮 spawn `opencode run`」改为「对接一个常驻 `opencode serve`」。其余 scrape-CLI driver 逐步退役（claude/codex/ollama/aider 的模型能力，本就是 OpenCode 已适配的 provider）。

### 1. 部署：BYO 安装 + 版本契约（不打包二进制）

沿用我们一直依赖 `claude` CLI 的同款模式——**用户自带 `opencode`**（`exec.LookPath` 已经在做）：

- **不**把 ~106 MB 的 opencode 二进制塞进 5 平台发布包（体积代价大、要核重发许可证）。
- 发版时**约定依赖版本**，写进 release notes + adapter 内一个常量；文档给安装入口（`brew install opencode` / 官方 install 脚本 + `opencode auth` 配 provider key）。
- provider 凭证由 adapter 注入给 serve（接 cloud_saas：用户在云端配，adapter 注入 env / 写 config），不让用户去碰 `~/.config/opencode`。

### 2. 运行时版本握手（不能只写文档）

因为消费的是**带版本的 HTTP API**（`EventSessionNext*`、part 结构在版本间会变），「约定版本」必须在运行时强制，否则用户装错版本会静默解析错位：

- 连上 serve 后第一件事打 `GET /global/health`（实测返回 `version`），与 adapter 钉的支持区间比对；
- 不在区间则**明确报错**（admin 页渲染「版本不符 + 安装命令」），不硬跑；
- 每次我们测过新版本，抬一次支持区间。

### 3. serve 进程托管

adapter 负责拉起并监管一个常驻 `opencode serve`（随机端口、健康检查、崩溃重启），用完不再每轮起进程——延迟更低、原生流式。

### 4. 接口映射（1.15.1 实测，driver.go ↔ OpenCode）

| driver.go | OpenCode 机制 | 状态 |
|---|---|---|
| `Start` / `Resume` | `POST /session`（立即得真 id）/ 复用 id 续发 | ✅ 比 scrape `step_start` 好 |
| `StartOpts.Model` + `ModelUpdater` | prompt 的 `model:{providerID,modelID}` | ✅ 白拿 75+ provider |
| `StartOpts.SystemPrompt` | UserMessage 原生 **`system`** 字段 | ✅ 原生，弃 `OPENCODE_CONFIG_CONTENT` hack |
| `StartOpts.MCPServers` | `POST /mcp` 运行时动态加 | ✅ MCP 一等公民，可热插 |
| `Approve` + `ToolApproval` | `EventPermissionAsked` → `POST /session/:id/permissions/:permissionID` | ✅✅ 从 false 变 true |
| `Interrupt` | `POST /session/:id/abort` | ✅✅ 现在根本没有 |
| `EvText` / `EvThinking` | `EventSessionNextText* / NextReasoning*`（token 级 delta） | ✅✅ 流式，thinking 现在只打日志 |
| `EvToolCall` / `EvTokens` / `EvTurnEnd` / `EvError` | `NextToolCalled` / `StepFinishPart.Tokens`(含 cache) / `SessionIdle` / `SessionError` | ✅ |
| `SessionLister` / `MessageLoader` / `PartLoader` | `GET /session` / `/session/:id/message` / parts(`Text`/`Reasoning`/`Tool`/`Subtask`Part) | ✅✅ 现在全缺，白拿，parts≈1:1 |
| `ModelLister` | `GET /config/providers`（带 `reasoning`/`toolcall` 能力位） | ✅✅ 替掉手维护 `models.go` |
| **`EvDispatchStatus` / `DispatchPart`（管家派活）** | 原生 **`SubtaskPart`** + `GET /session/:id/children` + `EventQuestionAsked/Replied` | ✅ 有原生原语，比 claude 上的 `mcp__bbclaw__dispatch` + scrape stream-json 更干净 |

结论：每个接口都能映射，且**白拿 5 个现在 opencode driver 没有的能力**（tool approval、interrupt、SessionLister、MessageLoader、PartLoader）。上一轮唯一标的风险（system prompt 注入）实测是原生 `system` 字段，已消除。这是**净升级**，不是 parity。

## spike 闸门（绿灯条件）

正式改 driver 前，写一个一次性 Go 程序连本机 `opencode serve` 验证 3 件事：

1. 发触发 Bash 的 prompt → `/event` 收到 **text + reasoning 的流式 delta**；
2. `EventPermissionAsked` 触发 → `POST .../permissions/...` 能放行；
3. `POST /mcp` 注册 `mcp__bbclaw__dispatch` → 对话冒出 **`SubtaskPart`**。

3 个全过 → 正式迁移。

## 实现状态（v1，2026-06-17）

spike 跑通核心闸门（GATE-0 版本握手、GATE-1a 流式 text delta），serve+SDK driver 已落地并通过 live 冒烟。

**spike 暴露、并已在实现中处理的两个真实坑：**
1. **`/event` 按 serve 启动项目作用域**，而 BBClaw 的 session 跑在任意项目目录 → 跨项目收不到事件。改用 **`GET /global/event`**（真全局流）。
2. **`/global/event` 把事件包在 `{directory, project, payload:{...}}` 信封里**（`/event` 不包）。router 解包 `payload` 后再按 type 分发。
3. SDK v0.19.2 落后 server 1.15.1（server 推 `message.part.delta`，SDK 没建模）→ 事件流**完全弃用 SDK 的 typed union,改裸 SSE 读 + 按 type 字符串/raw JSON 解**（SDK 只用于 session 生命周期、prompt、abort、permission 等请求/响应）。

**已落地（opt-in `AGENT_OPENCODE_SERVE=1`，旧 CLI driver 不动）：**
- `serveManager`：懒启动 + 监管常驻 `opencode serve`，`/global/health` 版本握手（支持区间 `[1.15, 1.30)`，越界拒绝并提示安装），崩溃自动重启。
- 流式 `EvText`/`EvThinking`（delta）、`EvToolCall`（显示）、`EvTokens`、`EvTurnEnd`、`EvError`、`EvInterrupted`。
- `Interrupt`（abort barge-in）、`Resume`、原生 `system` prompt 注入、per-turn model。
- 可选能力全实现：`ModelLister`（`/config/providers` 已认证 provider）、`SessionLister`、`CLISessionChecker`、`MessageLoader`、`PartLoader`（thinking/text/tool/dispatch）。
- **butler MCP 派发**（2026-06-17 补齐）：`StartOpts.MCPServers` 经 `POST /mcp` 注册到共享 serve（每个 server 名只注册一次，幂等）；butler 调 `bbclaw_dispatch` 工具时,router 把 tool part 的状态流映射成 `EvDispatchStatus`——`pending/running`→`started`(带 cwd/title)、`completed`→解析 output 的 `{status,taskId,elapsedMs,childSessionId}`(`running`→`async`)、`error`→error phase,完全对齐 claudecode 的 `mcp__bbclaw__dispatch` 语义;`PartLoader` 把历史里的 dispatch tool 也归为 `dispatch` kind。`system` persona 走原生 `system` 字段。
- 文件：`internal/agent/opencode/serve.go` / `serve_driver.go` / `serve_events.go` / `serve_models.go` / `serve_history.go` / `serve_dispatch.go`；测试 `serve_unit_test.go`（纯逻辑：版本/model/分页/parts/dispatch 识别/dispatch 解析）+ `serve_driver_smoke_test.go`（`OC_SMOKE=1` live：流式 + ModelLister/SessionLister + MCP 注册）。

**设备侧 tool approval（2026-06-17 补齐，opt-in `AGENT_OPENCODE_TOOL_APPROVAL=1`）：**
开启后 `Capabilities.ToolApproval=true`,`permission.asked`→`EvToolCall`(审批请求,ID=permissionID),设备 `Approve()`→`Session.Permissions.Respond`(`once`/`reject`);开启时抑制显示型 tool-part EvToolCall(避免与审批提示重复)。**默认仍关闭**(自动放行)——因为 `ToolApproval` 是 driver 级能力,若全局翻 true,无设备接管的会话(HTTP 直调、headless)会卡在等审批;opt-in 让审批管线完整可测而不冒挂起风险。

**provider 凭证注入（2026-06-17，P1-3 adapter 侧）：**
`opencode serve` 进程继承 adapter 的 `os.Environ()`,所以 admin/env 里配好的 provider key(`ANTHROPIC_API_KEY`/`DEEPSEEK_API_KEY`/…)**已自动流到 serve**——和 claude driver 拿 `ANTHROPIC_AUTH_TOKEN` 同一机制。额外提供 scoped 注入点 `Options.ProviderEnv`(经 `AGENT_OPENCODE_PROVIDER_ENV="K=V,K2=V2"`),用于 cloud_saas 下发**不进全局 env**的凭证(`buildServeEnv` 合并,provider 值覆盖继承值)。**cloud 侧**(把用户在云端配的 key 下发给 adapter)在 bbclaw-reference 私仓,属跨仓契约,不在本仓范围。

**版本不符 admin 提示（2026-06-17，P2-5）：**
环境检测(`detect.detectOpenCode`)跑 `opencode --version`,用 `opencode.ServeVersionCheck` 比对支持区间;不符时把 warning 写进 detection Data,经 `/v1/agent/drivers`(`driverInfoExtended.Warning`)+ `/v1/agent/environment` 透出,admin 驱动面板渲染「⚠ opencode：版本不在支持区间 + 安装提示」。不 disable 驱动(旧 CLI driver 仍可用),只警示。

**dispatch 子会话钻入（2026-06-17，P2-6）：**
serve 的 `PartLoader.mapParts` 对 dispatch tool part 解析 input(cwd/title)+ output(status/elapsedMs/**childSessionId**),完整重建 `DispatchPart`。admin 会话页本就按 claude 路径渲染 dispatch 卡片并支持 `childSessionId` 深链(api.ts `DispatchPart.childSessionId`),故 driver 侧填齐后钻入即通,无需改 web。

**fast-follow（未做）：**
- butler 派发的**端到端 live 验证**需可靠调工具的模型 + 真实 butlermcp server(dev box 的 deepseek 不稳定);现以「注册路径 live + 映射/解析单测」覆盖。
- 达到上述 parity 后再把 serve 后端设为默认、退役旧 CLI driver。

## 后果

**收益**
- driver 动物园 6 套 → **1 套**，只对接一个带版本的稳定 API，抗上游变更。
- opencode driver 从最弱变最强（流式 text+thinking、原生审批/中断、全历史回放、原生子任务）。
- 模型无关「白拿」：换模型改配置即可，对消费硬件（用户模型预算/访问权各异）很关键。
- 与 BBClaw「开源 / 不存代码」调性一致。

**代价 / 风险**
- 新增运维面：常驻 `opencode serve` 进程的监管（端口/健康/重启）。
- **版本漂移**：用户自带，版本不可控 → 由运行时握手兜底（决策 §2）。
- **单一上游耦合**：押 OpenCode 一家。缓解：开源、可自托管、API 带版本，远好于同时 scrape 多个闭源 CLI。
- `EventSessionNext*` 偏新：迁移前确认稳定性；稳妥兜底用 `EventMessagePartUpdated`。
- provider 凭证管理转由 adapter 承担（接 cloud_saas）。

**与 ADR-024 的张力（须评审）**
ADR-024（2026-06-10 接受）设的是「N 个自包含 driver 生态、激活后端任选」。本 ADR 把「多后端」下沉到 OpenCode 内部（它本就多 provider），adapter 侧收敛为单一 canonical 后端。管家编排层不变，变的是它底下后端的数量。此为**提议方向**，待与 ADR-024 一并评审后再定最终状态。
