# ADR-043 — 不抽公共 Agent 驱动 SDK,bbclaw adapter 与 agent-room 两边独立实现

- **状态**: 已接受（决定不做——记录被否掉的备选与理由）
- **日期**: 2026-07-01
- **关联**: ADR-035（v2 走交互式 PTY 抓屏,弃 `claude -p`,计费留订阅内）、ADR-031（OpenCode 作为 canonical 后端 / serve + SDK）、ADR-003（Router + 多 driver）
- **依据**: `chat/2026-06-30-cc-connect-adapter-enhancement-research.md`、`chat/2026-06-30-claude-code-stream-json-vs-tui-billing.md`（对 chenhg5/cc-connect 的调研）

## 0. 背景

我们有两处都要「驱动一个编码 CLI 做对话式接入」的需求:

- **bbclaw adapter**（本仓,设备主控制面）——v1 走 `claude -p` 一次性、v2 走交互式 PTY + 抓屏。
- **agent-room**（另一个独立仓库/项目)——同样要把本机 CLI 当对话底座用。

由此提出:能不能把「CLI 驱动内核」抽成一个公共 Go SDK（暂名 `clawagent`）,两边复用,甚至开源、让 cc-connect 这类项目接入?参考 [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect) 的 `Agent`/`AgentSession` 接口 + 注册表 + stream-json 长驻子进程模型。

## 1. 决策

**不抽公共 SDK。bbclaw adapter 和 agent-room 各自独立实现驱动层。**

- 不新建 `clawagent`(或 `cc-sdk` 等)公共 module。
- 不把 v1 的 `agent.Driver` 接口 / v2 的 `ptyhost`+`vtscreen`+`extract` 提取成对外库。
- 不以「让 cc-connect 接入我们的 SDK」为架构目标。

## 2. 理由

### 2.1 stream-json 已经是事实互通标准 —— 互通是协议级,不是依赖级

`claude --output-format stream-json --input-format stream-json` 是 cc-connect、官方 Claude Agent SDK、社区一堆 `claude-code-sdk-go` 共同说的协议。**只要消费者走 stream-json,互通就免费**,不需要任何一方 import 另一方的 Go 库。共享代码库带来的耦合收益 ≈ 0,维护成本却是实打实的。

### 2.2 我们唯一独有的只有 PTY transport,而它只有设备需要

去掉 stream-json（生态到处有现成实现),我们真正独有、别人没有的只有 **v2 的「交互式 PTY 抓屏保订阅计费」**（ADR-035 的核心价值)。而这条路:

- 只有 **bbclaw 硬件设备**因为订阅计费约束才真的需要;
- 对 agent-room 这类开发者自用 / API-key 计费无所谓的场景**没有必要**;
- `extract` 启发式会随每个 CLI 版本腐化,是长期维护坑,不适合当稳定公共 API 对外承诺。

因此没有第二个消费者需要「PTY transport 背后挂干净 Driver 接口」这件独家能力 → 抽成公共 SDK 缺乏理由。agent-room 若需要 stream-json,直接用成熟的现成 Go SDK 即可。

### 2.3 「让 cc-connect 接入我们」不可控,且方向反了

cc-connect 是外部仓库,我们无法让它 adopt 我们的 SDK,只能提 PR 等它接受;而我们唯一的独家（PTY 抓屏）恰恰是它最不想要的（脆、hacky、niche)。把硬件关键链路挂在别人的发布节奏上,风险大于收益。

### 2.4 内部复用不需要公共/开源库

即便将来两边真有共享需求,一个**私有 module** 就拿到 90% 好处,还省掉公共 API 稳定性、文档、issue 三项义务。开源只有在「把 PTY 保订阅计费当卖点引流」时才值得,当前不具备该动机。

## 3. 后果

- **正面**: 两边各自演进,零跨仓发布协调;bbclaw 设备链路(固件+adapter 协同发版)不被外部库牵制;避免为一个只有单一真实消费者的能力背公共 API 包袱。
- **代价 / 已接受**: 两边若都要 stream-json 驱动,会各写一份(但那份代码薄、协议稳定);driver 生态经验靠「选择性移植」而非共享库(延续既有做法,见 `chat/…cc-connect…` §3.3)。
- **互通仍在**: 需要与 cc-connect / 官方 SDK / 其它工具互通时,走 **stream-json 协议**对接,不引入代码依赖。

## 4. 重评触发条件

出现**第二个**明确需要「PTY 保订阅计费 transport」的非设备消费者时,重开此题、再评估抽私有 module。在此之前维持两边独立实现。
