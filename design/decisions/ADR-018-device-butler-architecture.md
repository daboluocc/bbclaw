# ADR-018: 设备管家(Butler)——Adapter 作为会话编排 + 记忆 + Claude 完整适配的中枢

- **日期**: 2026-05-30
- **状态**: 已接受（实施中——P0 turn 编排提取已落地，P1/P2 待实现）
- **关联**: ADR-001（Adapter as Agent Bus）、ADR-002（多轮会话生命周期）、ADR-003（Router + 多 driver）、ADR-014（Logical Session 抽象）、ADR-016（设备端 driver/model 选择）、`docs/PROJECT_PROFILE.md`、`design/agent_bus.md`、`design/multi_session_management.md`

## 背景

### 产品定位（前提）

沿用 ADR-014 与 `PROJECT_PROFILE.md` 的定位：BBClaw 是 **Claude Code / OpenCode / Aider / Ollama 等 Agent CLI 的语音外设**，形态接近 Siri / Alexa——用户从不关心后台是哪一个 conversation，只关心"我和我的 BBClaw 一直在聊"。CLI 实现细节（`cwd`、CLI session id、`--resume`）对设备**完全透明**。

本 ADR 把这个定位推进一步：**优先把 Claude（Claude Code CLI）做成完整适配的首选后端，让设备成为"CLI 终端的远程嘴/耳/屏"，并让 Adapter 从"透明 bus"升级为承载会话编排、记忆与项目画像的"设备管家(butler)"。**

### 问题诊断（2026-05-30 全链路剖析）

1. **复杂度外溢、职责放错层**：turn 编排逻辑在 `httpapi/agent.go`（LAN）与 `homeadapter/agent_proxy.go`（cloud）两份近乎逐行复制；设备端 `bb_ui_agent_chat.c`（约 3178 行）自带 session 列表分页、相对时间格式化、driver/model 切换状态机、历史回放游标等"本可由 adapter 算好下发"的展示逻辑。ADR-012 想消灭的 overlay 互斥矩阵在 `bb_radio_app.c` 的 nav 路由里复活。
2. **Claude 适配不足**：claudecode driver 只用了 `claude` CLI 约 10% 的能力（`-p` / `--output-format stream-json` / `--resume` / `--session-id` / `--model`）。未利用 `--append-system-prompt`、CLAUDE.md 项目记忆、MCP、`--permission-mode`、工具审批回路（`Capabilities.ToolApproval=false`，`Approve` 直接 `ErrUnsupported`）。
3. **记忆/画像≈空白**：adapter 仅持久化 `driver_state.json`（driver/model 偏好）与 `sessions.json`（logical session 路由元数据，无对话内容）。无任何"跨会话记住用户需求/对话/项目画像"的能力；CLAUDE.md 在全仓 0 命中。

## 决策

**总纲：薄设备(哑终端) + 厚管家(butler) + Claude 后端。** 设备退化为"PTT 录音 + TTS 播放 + 渲染 adapter 下发的菜单/对话"；adapter 承担会话编排、记忆/画像、Claude 能力注入；Claude CLI 仍是对话历史的事实存储与工具执行后端。

`logicalsession`（ADR-014）是管家的**会话路由核心**，但**不是记忆核心**——记忆/画像作为独立维度旁挂（会话级按 logicalID、项目级按 cwd 聚合）。

### 1. butler 包作为 turn 编排中枢（P0，已落地）

把两份重复的 turn 编排下沉到新包 `adapter/internal/butler`（`Engine.RunTurn`），通过 `EventSink` / `SessionRegistry` 接口、`MetricsSink` 语义化指标、策略开关（`ReuseWindow` / `AllowBareCLIID` / `AutoTitle` / `EmitTurnEndFrame` / `EmitStartFailedFrame`）与 hook（`OnStateChange` / `OnTurnComplete` / `OnFinalReply`）保留 LAN 与 cloud 各自的历史行为。**这是行为保持的纯结构提取**，两个 caller 净删约 447 行，且是后续 LLM 编排与记忆注入的**唯一挂载点**。

### 2. 记忆 / 项目画像 —— 优先复用 Claude 原生机制（决策：复用 Claude 原生为主）

不另造一套重存储；adapter 只存"提炼后的要点 + 画像"，对话全文仍留给 `~/.claude/projects/*.jsonl`。三类记忆 × 三条注入通道：

| 类型 | 存储 | 注入通道 | 写入时机 |
|------|------|---------|---------|
| 本地项目画像 | 每个 cwd 的 `CLAUDE.md`（adapter 生成/追加项目类型、常用命令、历史决策） | Start(cwd) 时 Claude 原生加载（headless 下可能需 `--add-dir`，见 §6） | turn 结束由 LLM 判定产生项目级稳定知识时 append |
| 用户需求记忆 | adapter 轻量存储（首选 SQLite），key=`(deviceId, project)` | `StartOpts.SystemPrompt` → `--append-system-prompt`（管家人格 + 设备约束 + 用户画像摘要） | turn 结束由 LLM 提炼增量更新 |
| 可检索长期记忆 | MCP memory server 后端 | `--mcp-config` 挂 MCP memory，让 Claude 主动 store/recall | 按需，避免塞满每轮 system prompt 致成本线性增长 |

**记忆维度**：先按 `device + project`（不动 cloud）。cloud 账户是"一设备多用户"（`account/store.go` Binding），"按 user 记忆"需把 user 维度下沉/同步给 adapter，列为后续。

### 3. Claude 完整适配 —— StartOpts 注入 + 常驻交互会话（决策：转向常驻交互会话）

- **P0 之二（低风险，先做）**：`agent.StartOpts` 新增 `SystemPrompt` 字段，claudecode 拼 `--append-system-prompt`；Start(cwd) 读取项目 CLAUDE.md。这是管家"有记忆/有人格"的最小可用版，**不改动现有 one-shot 进程模型**。
- **目标（决策#2）**：claudecode driver 从"每 turn cold-spawn `claude -p`"转向**常驻交互会话**（`--input-format stream-json --output-format stream-json`），以承载持续对话、工具审批往返。**但调研发现此路径有未文档化风险（见 §6），故拆成：先用 StartOpts 注入扛过早期，常驻会话作为打通动态审批的前置 spike。**

### 4. LLM 编排（决策：引入 LLM 编排）

butler 层引入一个"用 LLM 把设备意图翻译成 CLI session 操作"的编排器：解析设备抽象意图 → 规划选哪个 driver / 复用还是新建 session / 选哪个 cwd → 调 logicalsession 路由 → 注入记忆/人格。**注意延迟与成本**：LLM 编排不应无条件进对话主路径；先以"确定性规则（FindRecent 时间窗 + 项目类型映射）兜底、意图模糊时才调 LLM"的形式接入。

### 5. 工具审批 —— 暂用静态预授权（决策：暂不做动态审批）

- **当前**：用 `--permission-mode acceptEdits` 或精选 `--allowedTools` 白名单做**静态预授权**——既不是逐次问设备（动态审批），**也不是 `bypassPermissions`（全部跳过，危险，禁用）**。
- **动态审批（设备当 CLI 远程手，逐次 approve/deny）** 需要常驻交互会话 + 很可能需要 Agent SDK（`canUseTool` 回调；纯 Go+CLI 路径未证实），列为 §6 spike 后再定，属 P2。

### 6. 待验证的技术不确定项（spike 清单，进 P1/P2 前必须先答）

调研（claude-code-guide，2026-05-30）发现以下点官方未文档化或未证实，写死前须实测：

1. `claude --input-format stream-json` 的 stdin 帧协议官方**未文档化**（GitHub issue #24594）——常驻双向会话的可行性需逆向/实测。
2. CLI stream-json 模式下**工具审批请求是否会出现在 stdout 并能从 stdin 回注决策**？还是只有 Agent SDK 的 `canUseTool` 支持？这决定"动态审批"能否走纯 Go+CLI，还是必须引入 Node/Python SDK sidecar（与"单 Go 二进制 / 5 平台一键发布"的简洁性冲突，见 §影响）。
3. headless / `-p` 模式下 **CLAUDE.md 是否自动加载**，还是需 `--add-dir` 或显式注入？
4. 常驻会话模式下能否中途切换 `--model` / 改 system prompt？slash 命令（`/compact`）在 `-p` 模式不可用，上下文压缩须改用 `--exclude-dynamic-system-prompt-sections` 等 flag。

**已静态核实(`claude --help`, 2.1.156, 2026-05-30):**
- ✅ `--input-format` 取值 `text`(默认) / `stream-json`("realtime streaming input"，仅配 `--print`)——常驻双向【输入】确为受支持形态;但 stdin 的【帧 schema】仍未文档化,Q1 的帧级解析仍需实测。
- ⚠️ **无 `--permission-prompt-tool`、无 canUseTool/control 字样** → 当前 CLI **不提供动态逐次审批的 flag**;`--permission-mode` 取值仅 `acceptEdits/auto/bypassPermissions/default/plan`(静态预授权)。**结论:动态审批要么走 Agent SDK 的 canUseTool,要么维持静态预授权——验证了 §5"暂不做动态审批"的决定。** Q2 基本落定。
- ✅ **CLAUDE.md 加载已实测(CLI 2.1.158,2026-05-30)**:`-p` 下 CLAUDE.md **走 cwd 隐式加载**;`--add-dir <dir>` **不**注入该目录的 CLAUDE.md(只给工具访问权,实测 cwd=别处+--add-dir 读不到)。Q3 落定:用 **cwd**(driver 已设 `cmd.Dir=cwd`),不用 `--add-dir`。
- ✅ **MCP + `-p` 已实测**:`claude -p --mcp-config <file> --dangerously-skip-permissions` 能正确调用 adapter 托管的 MCP 工具并返回其值 → headless 下 MCP 工具可用(支撑 ADR-021 派发 server)。
- ✅ `--include-partial-messages`(token 级流式)、`--exclude-dynamic-system-prompt-sections`、`--append-system-prompt[-file]`、`--mcp-config`、`--fork-session`、`--no-session-persistence`、`--effort`、`--fallback-model`、`-n/--name` 均存在。
- 仍需【实测】(需真跑 claude,涉及 API 调用):stdin stream-json 帧的精确 schema;常驻会话中途改 model/system-prompt;cwd 是否在无 `--add-dir` 时也加载 CLAUDE.md。

## 影响

### 正面
- 设备显著变薄：session/cwd/driver/model 四个 picker 的上千行模板可塌缩成"渲染 adapter 下发的菜单 schema"。
- Claude 成为一等公民：人格注入、项目记忆、（后续）工具审批让设备真正成为"CLI 远程嘴/耳/屏/手"。
- 记忆/画像让"我和我的 BBClaw 一直在聊"从口号变成可落地能力，且优先复用 Claude 原生机制、跨 driver 可移植性靠 CLAUDE.md。
- turn 编排单点化，消除两份重复，降低跨 transport 漂移风险。

### 负面 / Tradeoff
- adapter 状态进一步变重（在 ADR-014 的 sessions.json 之上再加记忆/画像存储，倾向引入 SQLite）。
- 若动态审批最终必须走 Agent SDK sidecar，会给 adapter 引入 Node/Python 运行时依赖，**与 PROJECT_PROFILE §7 的"单 tag 双产物 / 5 平台 adapter 二进制"简洁发布模型冲突**——须在 spike 后专门权衡。
- LLM 编排若进主路径会增加延迟与 token 成本；必须以规则兜底、按需触发。
- server-driven 菜单需要重写固件渲染层（P0 之三），是一笔不小的固件改造。

### 中性
- ADR-014 logical session、ADR-016 driver/model 选择、ADR-013 history replay 的对外契约不变，butler 只是把编排收口。
- 协议层（设备↔adapter）短期不变；常驻会话 / WS 双向终端通道是后续协议演进项。

## 备选方案（已排除）
1. **adapter 自建 SQLite 作权威记忆库、Claude 只是消费者**：跨 driver 一致性最强，但要造一整套存储+检索+注入，且放弃 Claude 原生 CLAUDE.md 的零成本可移植性。降级为 MCP memory 的可选后端，不作主路径。
2. **动态审批用 `bypassPermissions` 一刀切放开**：让设备完全失去对危险操作（Bash/Edit）的控制，与产品定位相悖，禁用。
3. **LLM 编排无条件进对话主路径**：延迟与成本不可接受；改为规则兜底 + 按需触发。
4. **继续维持两份重复 turn 编排、就地加记忆/编排**：复杂度继续外溢，且每处都要改两遍，被 P0 butler 提取取代。

## 实现 checklist

- [x] **P0**: `adapter/internal/butler` 包——turn 编排提取（行为保持，两 caller 净删 ~447 行，build/vet/test 全绿）
- [ ] **P0 之二**: `agent.StartOpts` 增 `SystemPrompt`；claudecode 拼 `--append-system-prompt`；Start(cwd) 读项目 CLAUDE.md
- [ ] **P0 之三**: 固件删死骨架（`bb_page_chat.c`/`bb_chat_pickers.c`/`bb_chat_topbar.c`/`bb_chat_bottombar.c`）+ 四个 picker 改 server-driven 渲染器
- [ ] **P1**: 记忆写入/提炼管线（turn 结束挂 LLM 提炼 hook）+ 项目 CLAUDE.md 维护 + adapter SQLite 记忆存储
- [ ] **P1**: butler LLM 编排器（规则兜底 + 意图模糊时调 LLM）
- [x] **P1**: 统一默认模型双重事实来源（driver 默认改取 `claudeCodeModels[0]`，运行默认 sonnet-4-5→sonnet-4-6 对齐目录）；修 `logicalsession.Manager.Sweep` 失败无回滚
- [~] **spike**: §6 已核实——`--input-format stream-json` 存在、无 `--permission-prompt-tool`(动态审批需 SDK/静态预授权);**已实测(2.1.158)**:CLAUDE.md 走 **cwd 隐式加载**(非 --add-dir)、MCP 工具在 `-p` 下可调用;**仍待实测**:stdin stream-json 帧 schema + 常驻会话中途改 model/prompt(v2 常驻会话用)
- [ ] **P2**: 工具审批回路（依赖 spike 结论：纯 Go+CLI vs SDK sidecar）
- [ ] **P2**: MCP memory server 挂载 + 上下文压缩 / 成本追踪
- [ ] **P2**: WS 双向终端通道 + token 级流式（`--include-partial-messages`）
- [ ] **design**: 同步更新 `multi_session_management.md` 与 `PROJECT_PROFILE.md`
