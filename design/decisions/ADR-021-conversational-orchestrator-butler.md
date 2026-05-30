# ADR-021: 对话式编排管家 —— Claude 管家会话 + MCP 派发到 worker CLI Agent

- **日期**: 2026-05-30
- **状态**: 草案（架构方向已与用户敲定;有两个**前置实测闸门**,v1 范围明确)
- **关联**: ADR-018（设备管家 §4 LLM 编排 / §6 spike）、ADR-014（逻辑会话)、ADR-019（server-driven 菜单)、ADR-020（记忆管线——降级为本 ADR 的支撑件)、ADR-001/005（openclaw as driver)、`docs/PROJECT_PROFILE.md`

## 背景

### 产品愿景（用户口述,2026-05-30)

> 设备语音 → **管家(一个一直在跟你聊的 Claude)** → 听懂意图 + 搞清楚"在哪个工程目录、什么项目"(靠历史记忆或明确指令)→ **调起新的子 CLI Agent**(claude-code 等)在对应 cwd 跑任务 → 管家**当调度器/主管**,盯着子任务、按约定协议管它们,并用对话把进展/结果讲给你。

一句话:**管家是主管(对话+编排),worker 是工人(干活)。** 设备面对的会话是**管家对话**,真正的活在底下一个个 worker 会话里跑。这是 ADR-018 决策#4「LLM 编排」的中心化、加上"派发+监督子 agent"的版本。

### 现状不足

当前是「一次 PTT → 一次 `claude -p` → 回复」,设备直连**单个** claude 会话(ADR-014 logical session)。没有"管家在中间理解意图、跨项目派发、监督多任务"的一层。butler 包(ADR-018 P0)目前只做 turn 编排,不是编排器本身。

### 已锁定方向（用户拍板,本 ADR 在其内做具体设计)

排除"adapter 自建一整套 LLM agent loop(A)"(太重)。采用:**管家 = 一个 Claude 会话**(享 Claude 全套:思考/技能/上下文压缩),通过 **MCP**(用户认可)调 adapter 暴露的派发工具调起 worker。外壳按"常驻会话(C)"语义设计,**v1 用现有 `claude -p --resume` 实现**(复用 claudecode driver,零新进程模型),v2 升级常驻会话(gated on ADR-018 §6 spike,拿 /compact/流式/动态审批)。

## 决策

```
        ┌──────────┐  PTT/语音   ┌──────────────────────────────┐
设备 ───►│  管家会话  │◄──────────►│ Claude (cwd=~/.bbclaw-adapter/  │
(只跟    │ (butler) │  转述进展   │ workspace/, CLAUDE.md=人设+记忆) │
 管家聊) └────┬─────┘             └──────────────┬───────────────┘
             │ 设备只见管家             MCP 工具调用 │ dispatch/list_projects/task_*
             │                                    ▼
             │                    ┌──────────────────────────────┐
             │                    │ adapter MCP 派发 server        │
             │                    │  dispatch(project,task)        │
             │                    └──────────────┬───────────────┘
             │                       起 worker(复用 claudecode driver)
             │                                    ▼
             │            ┌─────────────┐  ┌─────────────┐  ┌─────────────┐
             └─不暴露──────│ worker A     │  │ worker B    │  │ ...         │
                          │ claude-code │  │ claude-code │  │ 各在目标     │
                          │ cwd=/proj/A │  │ cwd=/proj/B │  │ 工程 cwd    │
                          └─────────────┘  └─────────────┘  └─────────────┘
```

### 1. 管家会话（v1 用 `--resume`,复用 claudecode driver）

- 管家是一个 **per-device 的特殊 logical session**,`Driver="claude-code"`、`Cwd=~/.bbclaw-adapter/workspace/`、固定不变。设备每次语音 turn **永远路由到这个管家会话**(`handleAgentMessage` / 语音直发 `adapter.go:615` 不再让设备选 driver/session——设备只面对管家)。
- 持久:每 turn `claude -p --resume <butler-cli-sid>`(现有 driver.go:215 路径),会话上下文靠 `--resume` 跨 turn 持久;cwd=workspace 让 Claude 原生加载 workspace 的 CLAUDE.md(人设+记忆)。
- **WarmPool 预热管家会话**(pool.go,warmCwd=workspace):管家每 turn 都被命中,必须预热,否则每轮 4-7s 冷启动。
- v2:升级常驻交互会话(`--input-format stream-json`,ADR-018 §6 spike)拿 `/compact` 上下文压缩、流式、动态审批。
- ADR-019 的 driver/model 菜单语义:在管家模式下,设备不再选 driver(永远是管家);model 菜单可保留为"管家用哪个模型"。session/cwd 菜单语义被管家对话取代(用户用说的:"切到项目 X")。

### 2. MCP 派发 server（adapter 托管,管家经 `--mcp-config` 挂载）

adapter 起一个 MCP server(stdio 子进程或本地 SSE),管家会话 spawn 时加 `--mcp-config <配置>`。暴露工具:

| 工具 | 入参 | 出参 | 语义 |
|---|---|---|---|
| `list_projects` | — | `[{name, cwd}]` | 复用 CwdPool + 历史项目记忆,给管家"有哪些工程可选" |
| `dispatch` | `{project|cwd, task, wait_seconds?}` | 同步完成→`{status:"done", result}`;超时未完→`{status:"running", taskId}` | 在目标 cwd 起 worker 跑 task |
| `task_status` | `{taskId}` | `{status, progress?}` | 异步任务查询 |
| `task_result` | `{taskId}` | `{result}` | 取异步任务结果 |

- **dispatch 实现**:复用 claudecode driver / `butler.Engine`——在目标 cwd `claude -p "<task>" --permission-mode acceptEdits`(静态预授权,ADR-018 §5;worker 带工具会真实改用户项目),消费 Event 到 `EvTurnEnd`。worker 是 claude-code headless 自主跑完整 agentic 任务(多步工具调用),返回最终结果。
- **超时降级(关键)**:dispatch 阻塞至多 `wait_seconds`(默认 ~25s)→ 干完则**内联返回结果**;没干完则返回 `{status:"running", taskId}`,worker 转**后台异步**继续(见 §3)。短任务秒回,长任务自动转异步,设备不被长阻塞。
- **安全(关键)**:管家能 dispatch = 在用户机器任意目录跑 claude-code(= 任意命令执行)。**约束 cwd 必须在 CwdPool / 显式 allowlist 内**(`config` 的 CwdPool),拒绝任意路径;worker permission-mode 静态预授权,非 bypass。

### 3. Worker 生命周期 + 任务反馈协议

- **同步(短任务)**:dispatch 在 `wait_seconds` 内阻塞拿到 worker `EvTurnEnd` → 返回结果 → 管家天然知道完成(工具返回 = 用户说的"subagent 感知")。
- **异步(长任务)**:超时降级后,worker 在后台 goroutine 跑;完成时**复用现有通知**(`turn_end` / `session.notification`,notifications.go)回流。管家感知二选一:(a) 管家下一轮主动 `task_status`/`task_result`(用户问"好了吗");(b) **主动推送**:worker 完成 → adapter 给管家会话注入一条"task X 完成"的系统消息,管家把它转成一句话推给设备(复用设备通知通道)。v1 先做 (a) 轮询,(b) 主动推送作增强。
- **worker 输出回管家要裁剪/摘要**:worker 全 transcript 可能很大,只把**最终结果/摘要**回给管家,避免管家上下文被撑爆。
- **并发/调度**:v1 单 worker 串行(并发=1,与 WarmPool replenish 共享信号量,防抢资源);多任务并行作 v2。
- worker 会话**不暴露给设备**(设备只听管家转述);logicalsession 加一个 `Role`(butler|worker)字段区分,worker 会话不进设备 session 菜单。

### 4. Workspace + 人设预设 + 记忆（整合 ADR-020）

- `~/.bbclaw-adapter/workspace/` 布局:`CLAUDE.md`(管家**人设**openclaw 风格预设 + **长期记忆**),adapter 出厂带一份默认 CLAUDE.md;用户改动用 marker 段隔离(BEGIN/END BBClaw-managed)。
- **人设**(参考 openclaw 预设):管家是调度器、有 dispatch/list_projects 工具;设备约束(小屏/语音/PTT、简短可朗读、用用户语言);"复杂任务派给 worker、把进展讲给用户"。
- **记忆 = 每 turn 总结要点 append 进 workspace 记忆**(轻量版 openclaw):挂 butler turn 末 `Hooks.OnTurnComplete`,把"用户长期偏好 / 最近在做的项目 / 关键决策"追加进 workspace CLAUDE.md 的 managed 段(marker+上限+hash 去重),管家 --resume(cwd=workspace)时原生加载。这就是用户说的"总结对话记录到记忆"——也回答了之前"蒸馏是干嘛":在管家模式下它是"把对话沉淀成可持久记忆",供管家重启/换机后仍记得。
- **与 ADR-020 关系**:workspace CLAUDE.md = 管家长期记忆(本 ADR);各项目 cwd 的 CLAUDE.md = 项目画像(ADR-020 §3 仍独立);ADR-020 的"用户需求 → --append-system-prompt 摘要存储"在管家模式下**降级**——管家自己的对话上下文 + workspace 记忆已承担,不再需要单独的 memory.json 注入层(简化)。

## 前置实测闸门（动手前必须过)

1. **`claude -p --mcp-config X` 能否真的调用 MCP 工具?**(headless agent 模式下 MCP 工具可用性)——整个 §2 建立在此。未验证不写派发 server。
2. **`--add-dir workspace`(或 cwd=workspace)在 `-p` 下是否加载 workspace CLAUDE.md?**(承接 ADR-020 同款 spike)——人设/记忆加载的前提。

## 影响

### 正面
- 真正的"管家":用户用一个持续对话搞定多项目多任务,不用自己选 driver/cwd/session(设备彻底变薄)。
- 复用 Claude 全套能力(思考/技能/上下文压缩)做编排,不自建 agent loop。
- worker 复用现有 claudecode driver;记忆复用 CLAUDE.md;通知复用现有通道——新增面集中在 MCP server + 管家路由 + workspace。
- 与 ADR-019 菜单互补:低频选择(model)走菜单,意图/项目/任务走管家对话。

### 负面 / Tradeoff
- 多了一层 LLM(管家)→ 简单一问一答也要过管家,比直连单 claude 略慢/略贵(WarmPool + 管家可直接回答不 dispatch 缓解)。
- 派发 = 任意 cwd 命令执行,安全面扩大(allowlist + 静态预授权约束)。
- 长任务的异步反馈协议是新增复杂度(v1 先轮询)。
- MCP server 是新增的运行时部件(进程/配置/故障域)。

### 中性
- ADR-014 logical session 复用(管家/worker 都是 logical session,加 Role 区分);ADR-016 model 注入不变;butler.Engine turn 编排复用。

## 风险与缓解

| 严重度 | 风险 | 缓解 |
|---|---|---|
| high | `--mcp-config` + `-p` 能否调工具未实测,§2 全建立在假设上 | **前置闸门 1**,未过不写 |
| high | 延迟链:设备→管家(cold)→dispatch→worker(cold)→长任务数分钟,同步会让设备等爆 | dispatch **超时降级**(短任务内联、长任务转异步);管家 WarmPool;worker 异步 + 通知 |
| high | 安全:管家 dispatch 到任意 cwd = 任意命令执行 | cwd 限 CwdPool/allowlist;worker 静态预授权(非 bypass);拒绝白名单外路径 |
| medium | 管家上下文被 worker 长输出/多任务撑爆 | worker 只回摘要/最终结果;v2 靠常驻会话 /compact |
| medium | 管家冷启动每轮 4-7s | WarmPool 预热管家会话(warmCwd=workspace) |
| medium | worker hang / 不返回 | dispatch 超时 + worker ctx 超时 + 失败回管家(让它转述失败) |
| low | workspace CLAUDE.md 被 adapter 写 + 用户手写共存 | marker 段 + hash 幂等(同 ADR-020) |

## 备选方案（已排除)
1. **A:adapter 自建 LLM agent loop**:太重,放弃复用 Claude 原生编排。
2. **纯 B:只用 Claude Task 子代理**:Task 在管家自己 cwd 跑,无法原生跨到别的项目目录起 claude-code;跨项目 worker 必须走 dispatch 工具。
3. **只做常驻会话(C)不走 --resume**:被 ADR-018 §6 常驻会话 spike 卡住;v1 用 --resume 不阻塞。
4. **同步派发到底(不降级)**:长任务把设备阻塞数分钟,不可接受。

## 实现 checklist

**前置 spike**
- [ ] `claude -p --mcp-config` 能否调用 MCP 工具(headless)
- [ ] `--add-dir`/cwd 加载 workspace CLAUDE.md(同 ADR-020 闸门)

**v1（最小闭环:管家 + 同步/降级派发 + workspace 人设记忆)**
- [ ] workspace 脚手架:`~/.bbclaw-adapter/workspace/` + 默认 CLAUDE.md(openclaw 风格人设 + marker 记忆段)
- [ ] adapter MCP 派发 server(Go):`list_projects` / `dispatch`(超时降级)/ `task_status` / `task_result`;cwd 限 CwdPool
- [ ] 管家会话路由:设备 turn 永远路由到 per-device 管家 logical session(cwd=workspace);claudecode spawn 加 `--mcp-config` + `--add-dir workspace`;WarmPool 预热管家
- [ ] worker:复用 claudecode driver/`butler.Engine` 在目标 cwd 跑;同步阻塞拿 `EvTurnEnd`;输出裁剪回管家
- [ ] logicalsession 加 `Role`(butler|worker);worker 不进设备菜单
- [ ] 记忆:butler turn 末把对话要点 append 进 workspace CLAUDE.md managed 段(marker+上限+hash)
- [ ] 测试:管家路由、dispatch(同步/降级/cwd 白名单拒绝)、worker 生命周期、记忆 append 幂等

**v2**
- [ ] 异步主动推送(worker 完成→管家→设备通知)、多任务并发调度
- [ ] 管家升级常驻交互会话(/compact、流式、动态审批,ADR-018 §6)

## 需用户拍板
1. **派发反馈 v1**:同步 + 超时降级(短任务内联、长任务转异步轮询)——认可?还是 v1 就要主动推送?
2. **安全边界**:worker cwd 严格限 CwdPool / allowlist(管家不能在任意目录派发)——认可这个约束?
3. **记忆**:管家模式下砍掉 ADR-020 的 memory.json 注入层(workspace CLAUDE.md + 管家上下文承担)——认可简化?
4. **设备菜单**:管家模式下设备还保不保留 ADR-019 的 model 菜单(选管家用哪个模型),还是连这个也用说的?
5. **workspace 位置**:`~/.bbclaw-adapter/workspace/` ✓?(你已定)
