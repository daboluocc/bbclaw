# ADR-034: adapter_v2 计划清单（TodoWrite）→ 设备 display-only `task.list` 通道

- **日期**: 2026-06-25
- **状态**: 草案（设计定稿。**P0 第一步「采集探针」已落地**：`extract.ScanTaskListCandidates` + `deviceapi.captureTaskListProbe` 默认开、去重、记录每行前导字形 U+码点，`go test -race ./...` 全绿——用于在真机概率性命中 TodoWrite 渲染时留下原始数据，回头钉死字形→status 映射（§3）。待真机命中数据 → 严格识别器 + `task.list` 帧 + 固件渲染。）
- **关联**:
  - ADR-030（设备端执行步骤——display-only step 通道 / ignore-unknown 向后兼容）——**本 ADR 是它的范式延伸：把 `TodoWrite` 这个工具从单个 `tool.step` chip 提升为一条结构化清单通道**
  - ADR-033（adapter_v2 阻塞弹窗 → 设备确认）——**复用其「抓屏抽取 claude TUI 文本」方法论与「fixture 硬发布门 + parse 失败 fail-safe」纪律；但本特性是 display-only，无回路（不像 `prompt.select`）**
  - ADR-032（adapter_v2 会话生命周期——单根常驻 PTY 跑交互式 claude TUI，本 ADR 的抓屏对象）
  - ADR-029（对话页结构化 parts——thinking / text / tool / dispatch 分段）——**`task.list` 作为新 part 类型挂入对话页**
  - [ADR-021-firmware-ui](ADR-021-firmware-ui.md)（v1 管家「Task List」页）——**关键关系澄清见 §3：同一固件清单页概念，v1 用 butler-dispatch 结构化事件源，v2 用本 ADR 的抓屏 TodoWrite 源**
  - `adapter_v2/internal/extract/toolstep.go`、`internal/devicews/frames.go`（`tool.step`）——本通道的实现模板

## 背景

adapter_v2 用一根常驻 PTY 跑交互式 `claude` TUI（ADR-032），靠**抓屏抽取**把 claude 的输出翻成设备协议帧：`reply.delta`/`reply.end`（说）、`tool.step`（显示工具调用 chip，ADR-030）、`prompt.open`/`prompt.select`（阻塞菜单确认，ADR-033）。

claude 跑一个多步任务时，会调 **`TodoWrite` 工具**在 TUI 上渲染一张**计划清单**——每条一个待办，带状态（pending / in_progress / completed），随任务推进就地更新勾选。这正是用户想"像调用工具一样把进度展示到固件屏幕"的那种东西：

```
☐ 浙江金固拜访行前清单 – 第1条：精读年报
☐ 浙江金固拜访行前清单 – 第2条：读孙锋峰专访
☐ 浙江金固拜访行前清单 – 第3条：准备预研样章
☐ 浙江金固拜访行前清单 – 第4条：准备专利趋势图
```

**今天的缺口**：`TodoWrite` 也是个工具，所以当前 `extract.ScanToolSteps`（toolstep.go）只会把它抽成一个 `tool.step` chip `TodoWrite(…)`，**清单条目本身和勾选状态都丢了**——设备看不到"4 条里完成 2 条、正在做第 3 条"这种进度面板。

**为什么不是 ADR-021-firmware-ui 那条路**：见 §3。一句话——那是 v1 管家**派发 worker 子任务**的进度（不同项目、`mcp__bbclaw__dispatch`、结构化事件），adapter_v2 的单 PTY 模型里**根本没有 worker dispatch**；v2 里有意义的"任务清单"就是**当前这个 agent 给自己列的 TodoWrite 计划**，只能从 TUI 抓。

## 决策

新增一条 **display-only 的 `task.list` 下行通道**，把 claude 的 TodoWrite 计划清单整张快照推到设备渲染。严格沿用 ADR-030 的双通道契约：**只显示、永不朗读**（TTS 仍只由 reply prose 驱动）。

### 1. 通道契约（延续 ADR-030 双通道）

| 通道 | 帧 | 朗读 (TTS) | 显示 | 回路 |
|---|---|---|---|---|
| 说（主结果） | `reply.delta` / `reply.end` | ✅ | ✅ | — |
| 步骤（工具 chip） | `tool.step`（ADR-030） | ❌ | ✅ | 无 |
| **清单（计划进度）** | **`task.list`（本 ADR）** | ❌ | ✅ | **无（纯 display-only）** |
| 弹窗（需决策） | `prompt.open` / `prompt.select`（ADR-033） | ❌ | ✅ | 有（`prompt.select` 回 PTY） |

`task.list` 落在和 `tool.step` 同一象限：display-only、无回路，因此**复用 `deviceapi.Events` 插点**（新增一个 `TaskList(...)` 回调），不引入任何「设备 → adapter」上行消息。

### 2. 帧格式（下行，adapter → device）

```json
{
  "t": "task.list",
  "turnId": "t-12",
  "seq": 3,
  "items": [
    { "text": "精读年报",       "status": "done"    },
    { "text": "读孙锋峰专访",     "status": "active"  },
    { "text": "准备预研样章",     "status": "pending" },
    { "text": "准备专利趋势图",   "status": "pending" }
  ]
}
```

- `status` ∈ `pending | active | done`（映射自 TodoWrite 的 `pending | in_progress | completed`）。
- `seq` —— 每 turn 单调递增、独立于 `reply.delta` 的 `deltaSeq` 与 `tool.step` 的 `toolSeq`（devicews 侧 `deviceConn` 加一个 `taskSeq`，开 turn 时与其它 seq 一起清零）。
- **快照替换语义（非追加）**：每帧携带**整张当前清单**。TodoWrite 每次调用都吐全量数组，所以每次抓屏得到的就是一张完整快照；设备**整体替换**它的清单面板（类比 `reply.delta` 用全量文本替换字幕），不做 diff/append。
- **去重**：仅当清单内容（所有条目的 `text+status` 拼成的签名）相对上一次发出的有变化时才发帧——避免每个 PTY 输出 chunk 重发同一张表。参照 `tool.step` 的 per-turn 去重，只是 key 从单条 `{name,hint}` 换成整张清单的签名。
- **边界（安全/带宽，参照 `toolHintMax=80`）**：`items` 上限 16 条，超出截断并在末条标注 `+K`；每条 `text` 截断 48 runes（rune-safe，对齐 ADR-021-firmware-ui 的 title 截断 24 CJK / 48 字节）。320×172 屏一屏只放得下 ~4–5 行，固件按窗口滚动。
- **无显式清除帧（v1）**：与 `tool.step` 一致，不设 `task.list.clear`；清单的生命周期由固件挂在 turn 上（turn 结束后作为对话页的一个结构化 part 留存，见 ADR-029），或被下一张快照替换。后续若需要主动收起，再加空 `items` 约定。

### 3. 抓屏识别器（`extract/tasklist.go`，新增）

仿 `toolstep.go`：扫可见网格，识别**连续的待办行块**，每行 `<勾选字形> <文本>`，字形 → status 映射如下表。识别**故意严格**（像 `isToolStep` 那样），避免把普通 prose / 列表误判成清单。

| TodoWrite 状态 | 线上 `status` |
|---|---|
| pending | `pending` |
| in_progress | `active` |
| completed | `done` |

> **⚠️ 字形映射是 P0 闸门（未定，需真机探针）**：claude TUI 对 pending / in_progress / completed 各用什么字形（截图里 pending 是空心 `☐`；completed、in_progress 的实际字形/是否删除线/是否高亮**尚未在真机 fixture 中确认**），本 ADR **不臆测硬编码**。比照 ADR-033 的两个 gating 探针——先在真机（当前 claude 版本）抓一份 TodoWrite 渲染 fixture，把字形→status 映射钉死，再写识别器。识别器键于 claude 当前 TUI 文案/字形，**改版即失效**：以 fixture 回归（含对抗用例：普通 `- ` markdown 列表、`☐` 出现在 prose 里不应误判）作为**硬发布门**；**parse 失败一律 fail-safe = 不显示**（display-only，最坏只是少一块进度面板，无 TTS 误播、无安全后果）。

### 4. 与 `tool.step` 的去重（避免 TodoWrite 双重展示）

TodoWrite 本身是工具，会同时被 `ScanToolSteps` 抓成 `TodoWrite(…)` chip。**决策**：当某 turn 已识别出 TodoWrite 清单块并发了 `task.list`，**抑制那条冗余的 `TodoWrite(…)` `tool.step` chip**（在 `ScanToolSteps` 或 emit 层把 `TodoWrite`/`TodoRead` 名单加入 tool.step 黑名单）。设备只看到结构化清单面板，不再多一个无意义 chip。

### 5. 向后兼容（ignore-unknown，延续 ADR-030）

display-only + 无回路 ⇒ **无需能力协商**（不像 ADR-033 的 prompt 必须协商，否则远端会 hung-turn）：旧固件收到未知 `task.list` 直接忽略，不显示清单而已，绝不卡轮；旧 adapter 不发 `task.list`，新固件就是没清单可显示。新增字段对两端都安全。

## §3 与 ADR-021-firmware-ui「Task List」的关系澄清（设计冲突先解决）

两者**名字像、本质不同**，必须在纸面上分清，否则实现会撞车：

| 维度 | ADR-021-firmware-ui 的 Task List | 本 ADR (`task.list`) |
|---|---|---|
| 架构世代 | **adapter v1**（butler + MCP worker dispatch） | **adapter_v2**（单根 PTY 抓屏） |
| 语义 | 管家**派发给 worker 子 agent** 的子任务（跨项目） | **当前这一个 agent 给自己**列的 TodoWrite 计划步骤 |
| 数据源 | 结构化事件 `EvDispatchStatus`（`mcp__bbclaw__dispatch` 的 `tool_use`/`tool_result`）→ `/v1/butler/dispatch/recent` API | **抓屏** claude TUI 的 TodoWrite 渲染块 |
| 条目字段 | `{taskId, cwd, title, status: running/done/error/async, elapsedMs}` | `{text, status: pending/active/done}` |
| 回路 | 行 OK → 发 `task_status #<taskId>` turn | 无（纯进度展示） |

**结论**：adapter_v2 的单 PTY 模型里**没有 worker dispatch**，ADR-021-firmware-ui 的 dispatch 源在 v2 不存在。因此——

- **固件「清单页」UI 概念可共享**（同一个 LVGL 列表页、行高、空列表文案都可复用 ADR-021-firmware-ui §4.2 的视觉规格）；
- **但 v2 的数据源是本 ADR 的 `task.list` 抓屏通道**，不是 v1 的 `/v1/butler/dispatch/recent`。

即：本 ADR 给「设备清单页」提供 **adapter_v2 对应的数据源**。两份 ADR 不冲突，是同一 UI 在两代架构下的两个喂数路径；v2 不实现 v1 的 dispatch-recent 源。

## 分期（adapter-first，仿 ADR-033）

- **P0（adapter-only，可单独 tag）**：先做 §3 真机探针钉死字形映射 → `extract/tasklist.go` 识别器 + §4 tool.step 黑名单 + per-turn 清单去重 + 全套 fixtures（含对抗用例）。此阶段即便没有任何设备渲染也安全（Events 多一个 no-op 回调）。改了 adapter 抓屏行为 → 需 tag 才随发布出二进制。
- **P1（devicews LAN）**：`frames.go` 加 `task.list` + `deviceConn.TaskList` + `deviceapi.Events.TaskList(items)` 回调 + Bridge 在抓屏循环里 emit。设备近用户，先 LAN 打通渲染 UX。
- **P2（cloud + firmware，coordinated tag）**：cloud relay 透传（新增 event kind `task_list`，比照 ADR-030 `tool_call` 的 NDJSON pass-through，cloud 零逻辑改动）+ 固件清单页渲染（复用 ADR-021-firmware-ui §4.2 视觉规格 + ADR-029 对话页 part）。引入新 firmware 协议 → coordinated tag（CLAUDE.md「One tag, one release」）。

**Tag 策略**：本 ADR docs-only **不打 tag**；P0/P1 改 adapter wire → adapter 需 tag；P2 引入新 firmware 渲染 → coordinated tag。

## 跨组件协议同步（CLAUDE.md「Cross-Component Protocol Sync」）

| 改动 | 同步校验 |
|---|---|
| devicews `task.list` 下行帧 | Cloud hub 路由透传新 envelope kind（display-only，无回路，零逻辑） |
| cloudrelay `task_list` event 透传 | Cloud agent proxy NDJSON pass-through（比照 ADR-030 `tool_call`，原样透传） |
| `deviceapi.Events.TaskList` 回调 | devicews（LAN）+ cloudrelay（cloud）两个 Events 实现都要接 |
| firmware 渲染 `task.list` | Firmware WS/NDJSON handler 解析 + 新清单页（复用 ADR-021-firmware-ui §4.2 / ADR-029 part） |

## 风险

- **脆弱性**：识别器键于 claude 当前 TodoWrite TUI 字形/布局，claude 改版即失效 → fixture 回归（含对抗用例）作**硬发布门**，parse 失败 **fail-safe = 不显示**（display-only，无 TTS 误播、无安全后果，最坏少一块面板）。比 ADR-033 的弹窗抓屏**风险更低**：弹窗误判会卡轮/误批，本通道误判只是不显示。
- **字形映射未定**：§3 的真机探针是 P0 闸门，未跑探针不得写死映射。
- **与 tool.step 双重展示**：§4 黑名单未做会让 TodoWrite 同时出现 chip + 面板 → 实现时单测覆盖「TodoWrite 不再产生 tool.step」。
- **带宽/刷新**：TodoWrite 高频更新 → §2 去重（清单签名变才发）+ 条目/字数边界兜住。

## 后果

- 设备屏幕能像截图那样显示**带勾选状态的计划清单**，用户在跑多步任务时看得到「做到第几步」，而不只是孤立的工具 chip。
- 复用 ADR-030 双通道纪律：清单永不进说通道，TTS 行为零变化。
- 无回路、无能力协商、ignore-unknown：是四端里**最安全、最易增量上线**的一条通道。
- 把 ADR-021-firmware-ui 的「Task List 页」在 adapter_v2 下落地了数据源，澄清了两代架构同一 UI 的喂数路径分工。
- 代价：又多一处对 claude TUI 文案的脆弱依赖，以 fixture 硬门 + fail-safe-不显示兜底。
