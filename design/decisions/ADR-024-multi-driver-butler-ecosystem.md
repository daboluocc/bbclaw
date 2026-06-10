# ADR-024: 多-driver 自包含管家生态 —— persona 投影 + 同源 worker + 随 driver 的记忆

- **日期**: 2026-06-10
- **状态**: 已接受（设计定稿，分期实现）
- **取代**: ADR-023 的两条决策 —— ①「butler-capable = 仅 claude-code」改为 claude/codex/opencode 均可；②「通用 active_driver 与独立 butler_driver 两个设置」收敛为**单一激活 driver**（产品是管家-only，一个选择驱动全部）。ADR-023 的其余部分（环境检测、LookPath 门控、codex 驱动、driverstate 持久化、HTTP/云 parity）保留。
- **关联**: ADR-021（对话编排管家）、ADR-022（记忆整理管线）、ADR-018（设备管家架构）、ADR-016（驱动/模型选择）

## 背景

ADR-023 假设 claude 独占管家、worker 永远 claude、其它驱动只能降级。用户澄清了真实诉求,把这个假设推翻了:

1. **没有 claude 强依赖** —— 有的用户环境**只装 codex、或只装 opencode**,claude 根本不在场。管家必须能在任一驱动上完整运行。
2. **管家与 worker 同源** —— 管家派活时,worker 应是**与管家同款驱动**(codex 管家派 codex worker),属于同一套派发规则,而非永远 claude。
3. **复用各驱动原生生态** —— claude 用 `CLAUDE.md`、codex/opencode 用 `AGENTS.md`,各自的 skills 目录、配置等都**原样复用**,我们不另造一套、不隔离。
4. **adapter 持有一套 persona/基础知识** —— 在这些原生生态**之上**,adapter 定义唯一一份 persona + 基础知识(「约定的那几个文件内容的说明」),初始化时**投影**进不同驱动的原生文件,使各驱动复用同一套基础知识。
5. **跨驱动不共享上下文** —— 切换驱动 = 换一套全新会话 + 该驱动自己的记忆,**故意不做迁移**以降低难度。
6. **蒸馏/记忆管理随激活驱动** —— ADR-022 的蒸馏/整理管线也按**激活驱动**派发任务,不写死 claude。

## 决策

**驱动是自包含的生态单元;adapter 是其上的编排层,持有唯一 persona/基础知识真相源,把它投影到激活驱动的原生承载里;其余(worker、记忆、skills、配置)全部跟随激活驱动,跨驱动互不共享。**

### 1. 单一激活驱动驱动全部
取消 ADR-023 的「通用 / 管家」两设置。**一个**「激活驱动」决定:管家后端、worker 后端、记忆蒸馏后端、persona 投影目标。`driverstate` 保留 `active_driver`(单一字段);`butler_driver` 字段废弃/合并。

### 2. adapter 持有 canonical persona + 基础知识,init 时投影进原生文件
- adapter 内置唯一一份 BBClaw 管家 persona(对讲机式短回答、设备控制提示、form-factor 约束)+ 基础知识。
- 管家会话初始化时(`EnsureButler` 建会话时),adapter 把这份内容**投影**进激活驱动的原生指令文件,放在**托管块**内(增量叠加,不清掉用户内容):
  | 驱动 | 投影目标 |
  |---|---|
  | claude | workspace `CLAUDE.md` 托管块 / `--append-system-prompt` |
  | codex | workspace `AGENTS.md` 托管块 / `-c model_instructions_file` |
  | opencode | `instructions` / `OPENCODE_CONFIG_CONTENT` 内联 agent prompt |
- 管家跑在**隔离 workspace cwd**(ADR-021 §3),在那里写 `CLAUDE.md`/`AGENTS.md` 安全。**worker 跑在用户项目 cwd,不投影、不改用户文件**——worker 复用用户项目自己的原生文件 + skills(决策 4)。

### 3. worker 跟随激活驱动(同源派发)
`butlermcp/runner_claude.go` 的写死 claude 改为**按激活驱动参数化**的 worker runner。各驱动配各自免审批 flag:claude `--permission-mode acceptEdits`、codex `--full-auto`、opencode `--dangerously-skip-permissions`。派活 MCP server(`mcp__bbclaw__dispatch`)经 `BBCLAW_WORKER_DRIVER` 得知用哪个驱动起 worker。

### 4. 复用原生生态:增量叠加,不隔离
与 ADR-023 复验里「`--ignore-user-config` 隔离全局」**相反**:管家**继承用户原生配置**(skills、自定义 MCP server),只**额外叠加**我们的派活 server。即「用户那套原封不动 + 我们加一个派活工具」。(凭据仍走子进程 env、不进 argv。)

### 5. 派活 server 用格式中立 spec
`StartOpts.MCPConfig`(claude 专属 JSON 路径)→ `agent.MCPServerSpec{Name,Command,Args,Env}`,各驱动渲染各自格式:claude→`--mcp-config` JSON、codex→`-c mcp_servers.bbclaw.*`、opencode→config `mcp` 块(工具名各异:claude `mcp__bbclaw__dispatch`、opencode `bbclaw_dispatch`、codex `bbclaw` mcp_tool_call——各驱动的派活状态解析需各自匹配)。

### 6. 记忆/蒸馏随激活驱动,per-driver 不共享
ADR-022 的蒸馏→整理管线**把任务派给激活驱动**(codex 管家 → codex 蒸馏)。每驱动各自记忆存储;切驱动 = 换该驱动一套记忆。不做跨驱动记忆迁移/合并(决策 5)。

### 7. Capabilities.Butler
claude/codex/opencode = `true`;openclaw = `false`(WS relay,工具服务端跑,无本地 cwd/原生文件生态);ollama/aider 暂不在多-driver 管家范围(无原生 persona/skills 生态)。

### 8. 接口不变
`agent.Driver` 接口不动。抽象是**数据**(`StartOpts.SystemPrompt` + `MCPServers`)+ **adapter 侧 persona 投影** + **各驱动私有渲染**,不加接口方法。

## 已验证 / 待验证（来自 ADR-023 设计 workflow + 对抗复验）

- ✅ 已实测:codex `-c mcp_servers.*`、opencode `OPENCODE_CONFIG_CONTENT` 均能 **per-invocation** 注册派活 server、完成握手、注册工具、注入 env;persona 投影机制(codex AGENTS.md/`model_instructions_file`、opencode instructions)可用;codex `exec` 读 stdin 需重定向 `/dev/null`。
- ⚠ **待真机复验(本机模型凭据受限,验不了)**:codex、opencode 的「模型真实发出 `tools/call` 调用派活工具」这最后一跳。在有可用模型凭据的环境复验通过前,codex/opencode 的 `Capabilities.Butler` 藏在 `AGENT_<DRIVER>_BUTLER_VERIFIED` 后,默认 false。

## 跨组件

- homeadapter 云 relay:单一 active_driver(去掉 butler_driver.set)、worker driver 随之。
- 固件菜单 `set_driver`:现在切的就是「整套生态」,语义更直白。
- Web admin:**单一驱动选择器**(取代 ADR-023 两段式),未装灰掉。

## 分期

- **Phase 0** — `MCPServerSpec` 重构(claude 字节级不变);收敛单一 active_driver。
- **Phase 1** — persona 投影机制(adapter canonical persona → 各驱动原生文件托管块);worker runner 按激活驱动参数化。
- **Phase 2** — opencode 接入(persona + 派活 + `bbclaw_dispatch` 解析),门控。
- **Phase 3** — codex 接入(AGENTS.md/`model_instructions_file` + `-c mcp` + `--ignore-user-config`... 注意决策 4 是「不隔离」,codex 这里需权衡:派活 server 用 `-c` 叠加但保留用户全局 + stdin `/dev/null`),门控。
- **Phase 4** — 蒸馏/记忆管线按激活驱动派发,per-driver 存储。

## 范围外 / 已知限制

- ollama/aider 暂不作多-driver 管家(无原生 persona/skills 生态)。
- 跨驱动上下文/记忆**不共享**(决策 5,故意)。
- codex 决策 4(复用全局)与「派活 server 隔离」有张力,Phase 3 单独权衡。
