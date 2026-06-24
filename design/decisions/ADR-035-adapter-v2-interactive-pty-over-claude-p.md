# ADR-035: adapter_v2 跑交互式 PTY + 抓屏，而非 `claude -p --output-format stream-json`

- **日期**: 2026-06-25
- **状态**: 已接受（追认既有架构 —— 把散落在代码注释里、ADR-032 一句带过的「弃 `claude -p`」WHY 钉成正式决策）
- **关联**:
  - ADR-032（adapter_v2 会话生命周期 —— 单根常驻 PTY 跑交互式 claude TUI）：**本 ADR 补上它「弃 `claude -p`」那句话指向的、原本悬空的 `[[adapter-v2-pty-refactor]]` 缘由**
  - ADR-031（OpenCode 作为 canonical 后端）：本 ADR 解释为什么 v2 **没有**走 v1「每 CLI 一个结构化 driver」的收敛路线，而是 CLI-无关的抓屏
  - ADR-030 / ADR-033 / ADR-034（display-only step / 阻塞弹窗 / 计划清单）：都是**本决策的下游**——每条设备协议帧都从「抓屏抽取 claude TUI」翻译而来
  - ADR-018（设备管家）：抓屏抽取喂的就是管家会话
  - `adapter_v2/internal/extract/`（抽取层 + `CASES.md` 情况对照表）、`internal/vtscreen/`（进程内终端模拟器）：本决策的实现与脆弱点所在

## 背景

adapter_v2 的设备/语音线后端有两条技术路线：

| 路线 | 怎么拿回复文本 | v1 / v2 |
|---|---|---|
| **A. headless 结构化** | `claude -p --output-format stream-json --verbose`，claude 吐结构化 NDJSON，回复是干净字段 | v1 用（`adapter/internal/agent/claudecode/driver.go`） |
| **B. 交互式 + 抓屏** | 一根常驻 PTY 跑**交互式** claude TUI（`--resume`/`--continue`/`--session-id`，**不带 `-p`**），用进程内终端模拟器（`vtscreen`/vt10x）把 ANSI 字节流解析成字符网格，再抽取（`extract`） | **v2 用** |

纯从工程整洁度看，路线 A 完胜：结构化、不用逆向人类向 TUI、没有「回复比终端高就滚屏丢字」这类问题（本仓 2026-06-25 修的长清单 TTS 念不全 bug 就是路线 B 的副作用，见 `extract/CASES.md` C9）。那 v2 为什么明知更脏还选 B？

## 决策

**adapter_v2 坚持路线 B：驱动交互式 claude TUI + 抓屏抽取，刻意不用 `claude -p`。** 两条硬理由，都不是工程整洁度能换的：

### 1. 计费独立性 —— 不踩 `claude -p` 可能的独立收费口子

`claude -p`（print / headless 一次性调用）有可能被官方按**独立于交互式订阅的另一套方案计费**（例如按调用走 API 额度，而不是走用户那份交互式订阅）。BBClaw 的设备/语音线如果走 `-p`，等于每一轮对话都可能触发这套独立收费——成本不可控，且把用户已有的订阅额度晾在一边。

驱动**交互式** claude（和人坐在终端前敲的是同一个进程、同一份会话、同一套订阅），让设备的每一轮就是「人在用 claude」，**留在交互式/订阅计费里**，不触发 `-p` 那条独立收费路径。这是产品成本结构的底线，排在工程整洁度之前。

### 2. 多 CLI 无缝兼容 —— 抓屏是 CLI-无关的

路线 A 要求每个后端 CLI 都提供 `stream-json` 这种结构化输出协议；不同 CLI 协议各异，每接一个就要写一个专用解析 driver（v1 的「driver 动物园」，ADR-031 正是为收敛它而来）。

路线 B 抓的是**人类向 TUI 的最终渲染文本**——任何能在终端里跑、把回复画到屏幕上的 agent CLI，都能用**同一套** `vtscreen` + `extract` 驱动，无需它额外提供机器可读协议。这让 adapter_v2 后续**无缝兼容多 CLI**（claude 之外的各家 agent CLI）成本极低：换 CLI ≈ 换 argv + 调抽取启发式，不是从头写 driver。

## 后果

**接受的代价**：
- 抽取层（`extract`）是 v2 里**最脆弱的一层**——在逆向一个本是给人眼看的 UI。所有「TUI 行为变了就出 bug」的问题都集中在这：spinner 重绘、`--resume` 状态块伪装成 `⏺`、回复比终端高滚屏丢字（C9）、多段顶格布局等。
- 因此立下纪律：**抽取的每个分支都必须有一条 case 兜底**，全部登记在 `adapter_v2/internal/extract/CASES.md`（情况对照表），改抽取逻辑前先在表里登记 case 再动代码。这正是「这个适配器的函数开发要记录每个例子的情况处理」的落地。
- 抓屏只对 **primary screen** 有效；若某 CLI 走 alt-screen（`?1049h`）全屏 TUI，`vtscreen` 不留 scrollback，抽取会受限——接入这类 CLI 时需单独评估（见 CASES.md「已知边界」）。

**换来的好处**：成本留在订阅内；一套抓屏管线吃多家 CLI；顺带白拿一个「可被 web 终端 xterm.js 实时旁观、多轮 `--resume` 连续、TUI 内授权弹窗原样可见」的活会话（ADR-032/033 都建立在此之上）。

**不选路线 A 的代价**（即放弃了什么）：干净的结构化文本、天然不截断。**若将来计费政策变化使 `-p` 不再独立收费，或某后端只在 headless 模式可用**，可针对**单条语音回复文本**起一个旁路结构化通道（把「念什么」与「显示什么」解耦），而 PTY 仍留作人看的实时视图——届时重评本 ADR。
