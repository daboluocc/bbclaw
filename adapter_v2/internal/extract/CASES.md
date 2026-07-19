# extract 情况对照表（Reply 提取 case catalog）

> 本表是 `internal/extract` 提取函数的**情况清单**：每条 = 一种屏幕状态 → 期望的提取结果 → 覆盖它的测试。
> 新增/修改提取逻辑时，**先在这里登记 case**，再写代码 + 测试，方便后续拓展与回归对照。
> 提取逻辑本身是 v2 里最脆弱的一层（逆向 claude 的 TUI），所以每个判断分支都必须有一条 case 兜底。
> **为什么要抓屏、而不是用 `claude -p` 的结构化输出**：见 [ADR-035](../../../design/decisions/ADR-035-adapter-v2-interactive-pty-over-claude-p.md)（计费留在订阅内 + 多 CLI 无缝兼容）。本表就是那条决策「接受抽取脆弱性」所立的纪律。

## 背景：提取走哪条路

`extractRaw()`（[extract.go](extract.go)）按优先级三条路：

1. **marker 路径** `extractMarkerBlock(markerSource)` —— 锚定**最后一个真正的** `⏺` 回复块。
   `markerSource = ScrollbackText(200) + VisibleText()`（**C9 起改为含 scrollback**）。
2. **claude chrome 抑制** `isClaudeScreen(visible)` —— 没有 `⏺`、但屏上有 claude 的 `❯`/spinner/盒线 → 返回 `""`（还没出回复，别把欢迎页当回复念出去）。
3. **baseline diff 兜底** —— 仅给**没有 `⏺` 标记的 CLI**（aider/裸 CLI）用：可见内容减去 `New()` 时的 baseline。

边界（turn 何时结束）是另一个函数 `boundary.go::Detector`，case 见 [boundary_test.go](boundary_test.go)，不在本表。

## Case 清单

| ID | 场景 | 输入屏幕状态 | 期望提取 | 覆盖测试 |
|----|------|--------------|----------|----------|
| C1 | 标准单轮回复 | 录制的真实 claude TUI 流（prompt + 上一轮 + 本轮回复 + spinner） | 仅本轮干净回复文本；不含用户 prompt、spinner、上一轮 | `TestExtractFixtureMatchesAnnotation` |
| C2 | spinner 重绘不抖动 | 回复已出，之后 50 帧 spinner 原地重绘 | 提取文本不变、`changed=false` | `TestSpinnerRedrawsNoJitter` |
| C3 | 流式单调增长 | 回复逐块喂入，夹杂 spinner 帧 | 文本只增不减、不丢已出行、无重复行；spinner 帧 `changed=false` | `TestStreamingMonotonic` |
| C4 | 隔离最新一轮 | `New()` 时上一轮 `⏺` 已在屏 | 只出最新轮，旧轮被 baseline 排除 | `TestBaselineIsolatesNewestTurn` |
| C5 | 出回复前抑制 chrome | claude 欢迎/idle 盒线 + `❯`，**无** `⏺` | `""`，直到 `⏺` 出现 | `TestExtractSuppressesClaudeChromeUntilMarker` |
| C6 | --resume 状态块伪装成 `⏺` | 真回复后又渲染 `⏺ [Opus…] │ workspace`、`⏺ ⏵⏵ bypass…` | 锚定最后一个**真**回复，跳过 `⏺ <chrome>` | `TestExtractSkipsResumeStatusMarkerBlocks` |
| C7 | 全是状态 `⏺`、没真回复 | 只有 `⏺ [Opus…]` 等状态块 | `""`（别念 footer） | `TestExtractStatusOnlyMarkersYieldNothing` |
| C8 | 无 marker 的 CLI | 没有盒线/`❯`/`⏺` 的裸回复行 | diff 兜底返回该行 | `TestExtractFallbackStillWorksForMarkerlessCLI` |
| C9 | **回复比屏幕高（长列表）** | 回复行数 > 终端可视行数，顶部含 `⏺` 锚点已滚出可视网格进 scrollback | **完整回复**（含已滚出的标题与开头各项），不被截断成可见尾部 | `TestExtractRecoversReplyTallerThanGrid` |
| C10 | 多段、后续段落顶格 | `⏺ 段一` + 空行 + 顶格`段二` + `✻ … for Ns` | 整段回复（段一+段二），止于 `✻` 完成行 | `TestExtractMarkerBlockFlushLeftParagraphs` |
| C11 | **token 计数贴在回复行尾** | 短回复同一行尾部带 token chrome：`⏺ 谢谢…吩咐。  ↓ 3 tokens)` | 锚定该行为**真回复**（不当噪声丢弃），且 token chrome 被 `NormalizeReply` 剥掉不念 | `TestExtractReplyWithGluedTokenCounter` / `TestNormalizeReply` / `TestIsNoiseLine` |
| C12 | **effort 脚注伪装成回复气泡** | claude 2.1.x 输入框脚注 `● high · /effort`（彩色圆点开头），恰为屏幕上最后一个 `●` 行 | 锚定跳过该行念真回复；仅脚注在屏时返回 `""`（真机 bug：设备念出 "high · /effort"） | `TestExtractSkipsEffortFooterHint` / `TestExtractEffortFooterOnlyYieldsNothing` / `TestIsSlashHintLine` |

## C9 详记（本次修复 —— TTS 念不全的根因）

**现象**：长清单回复（如 8 条行前清单），设备 TTS 只念了一部分，但 web 终端/会话页文字是全的。

**根因**（当时）：
- 默认会话 PTY 当时是 **80×24**（`DeviceSession.Config()` 未设 `InitialSize` → ptyhost 默认 24 行），且 web 终端一 resize 就把共享 PTY 压到浏览器视口大小。
- claude 交互 TUI 里，回复一旦超过可视行数，顶部（含 `⏺` 锚点）滚出可视网格进入 scrollback。
- 修复前 `extractMarkerBlock` 只读 `VisibleText()`（可视网格），找不到 `⏺` → 退化到 diff 兜底只拿到**可见尾部**，甚至返回 `""`。
- 这段被截断的文本就是发给云端 TTS 的 `voice.reply`（[cloudrelay/transcript.go](../cloudrelay/transcript.go)）→ 设备只念到尾部几条。
- web 会话页回放的是完整 PTY + scrollback（连 `✻ Brewed for Ns` 都在），所以**文字全、TTS 缺** —— 数据源不同。

**修复（两层，叠加）**：
1. **抽取兜底**：marker 路径改读 `ScrollbackText(200) + VisibleText()`（[vtscreen.ScrollbackText](../vtscreen/vtscreen.go) 新增），锚定最后一个 `⏺`，把滚出去的回复头补回来。短回复不受影响（scrollback 此时只含旧轮，落在当前 `⏺` 之前被忽略）。
2. **从源头少滚屏**（ADR-035）：默认会话 PTY 改为**固定大网格** `session.DefaultGridCols×Rows`（当前 **120×60**），且**不再支持运行时 resize**——web 终端是 CSS 框定的固定尺寸观看器（[termchan](../termchan/termchan.go) 忽略 resize、[TerminalView.vue](../../web/spa/src/views/TerminalView.vue) 按服务端报的网格定 xterm）。现实长度的回复基本一屏装下，根本不滚屏，第 1 层只在极端超长时才兜底。

**边界与已知限制（后续 case 候选）**：
- 只对 **primary screen** 生效：claude 走 alt-screen（`?1049h`）时 vtscreen 不留 scrollback（设计如此）。当前 claude 内联渲染走 primary，reconnect 历史也依赖这点。若未来某版 claude 改用 alt-screen，C9 失效 → 需新 case。
- scrollback 仅在**真实换行滚动**时捕获（`captureScroll` 的 `\n` 前后 diff）；若 claude 整屏重绘而非增量滚动，滚出的行不入 ring。需用真实长回复 fixture 复核。
- 兜底（C8，无 marker 的 CLI）仍是**可见网格 only**，长回复仍会截断 —— 设备路径是 claude（有 marker），暂不影响；如要支持裸 CLI 长回复，需把 scrollback 也并入 baseline diff（注意别把旧轮当新内容）。
- `markerScrollbackLines = 200`：语音回复天然短，200 行足够覆盖滚几屏的长清单；锚定最后 `⏺` 使更早的历史无害。

## 怎么加新 case

1. 在本表加一行（ID 顺延、场景、输入、期望、测试名）。
2. 在 [extract_test.go](extract_test.go) 加同名测试，注释写清**为什么**（哪个真机 bug / 哪种 TUI 行为）。
3. 若涉及 scrollback / 网格几何，参照 C9 用小网格 `vtscreen.New(cols, rows)` 构造，真实 `\r\n` 触发滚动。
4. `go test ./internal/extract/...` 通过后，在本表 C9 那种"详记"段落补根因与边界（如属重要真机 bug）。
