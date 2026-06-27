# ADR-041 — 回合取消语义 + 权威回合状态机（撤销=干净中止·全新重发）

- **状态**: Accepted（§0 串轮 fix 已实现 + 真机验证;§5 权威状态/撤销标记为后续 P2/P3）
- **日期**: 2026-06-27
- **关联 / 修订**: ADR-028（对话核心 v2 / barge-in），ADR-040（设备↔adapter 回合同步），ADR-035（PTY 抓屏）
- **依据**: 2026-06-27 真机日志 + `cmd/cancelprobe` 对真 claude 的实测（见 §0）

## 0. 实测修正（2026-06-27，PTY 探针）—— 推翻 §1/§4 的「redirect」模型

> ⚠️ 下文 §1「病根=ESC 进重定向态、需要 dismiss 键」和 §4「检测 INTERRUPTED + dismiss +
> 跑在 Run goroutine 的取消状态机」**是基于一个错误模型**。用 `cmd/cancelprobe` 对真
> claude(v2.1.158)逐帧实测后,真相和修法都简单得多。本节为准。

**实测事实：**
1. **一个 ESC 就是干净中止。** ESC 后 spinner 消失、**composer 立刻清空**(裸 `❯`),
   上一句**没有**残留在输入框里。
2. **「Interrupted · What should Claude do instead?」是被动的历史标记**(行首 `⎿`,
   留在 scrollback),**不是**会拦输入的弹窗。第二个 ESC、Ctrl-C 都**不会**消除它,
   也**不需要**消除——直接打字就是干净的新一轮。
3. **串轮的真因是注入时机 + composer 追加**:adapter 旧逻辑 barge-in 是
   `ESC + 60ms + "transcript\r"`。**60ms 太短**,claude 还在处理打断,这一句的回车
   没提交、文本滞留在 composer;**紧接着的下一次 barge-in 把它的文本 APPEND 上去** →
   合成一句 `「啊…再讲…一二三」` 喂给 claude(实测复现:`MANGO.Reply…KIWI`)。
   与「重定向态」无关。

**修法(已实现 + 探针验证):barge-in 注入序列改为**
`ESC → 等 interruptSettle(250ms) → Ctrl-U(清空 composer) → 写 transcript → 停 injectPause → 单独写 Enter`。
- Ctrl-U 保证「永远往空 composer 里打字」,滞留的上一句被清掉 → 最新一句**替换**而非合并
  (= 用户要的「撤销=全新重发」)。
- 单独的 Enter 防 claude 的 burst/paste 检测把回车当换行。
- **不需要**:检测 INTERRUPTED 态、dismiss 键、Run-goroutine 取消状态机、screen 轮询。
  纯写端改动(`deviceapi.go` SubmitVoiceTurn barge-in 分支),~15 行。

**因此撤销/修订：**
- §1 的「ESC=redirect」叙述、§4 的「INTERRUPTED 检测 + dismiss + 状态机」、§6.5 的实现
  约束 —— **全部作废**,以本节为准。
- 之前提交的 boundary `interruptedOnScreen→TurnEnded false` 检测 **已 revert**(那个历史
  标记会长期留在 scrollback,会错误地永久压住后续 turn-end)。
- **仍然有效**:§5 的「就绪要权威(等 turn.idle)+ 撤销回合标记『已撤销』」—— 这俩是独立的
  跨端工作(P3 固件 + turn.cancelled/idle 事件),不受本节影响,留作后续。

## 1. 病根（早期假设 —— 已被 §0 实测推翻，保留作记录）

实测 claude TUI 在被 ESC 打断后：
```
…给你讲一个:从前有只小狐狸…跑起
Interrupted · What should Claude do instead?      ← ESC ≠ 中止,而是"重定向"态
❯ 取消吧。                                          ← 下一句被灌进重定向框
好,取消了。                                         ← 被当成"改干这个"在同一轮里执行
```

**核心结论：claude 的 ESC 是「打断并问你改干什么(redirect)」，不是「干净中止并丢弃(abort)」。**
adapter 现状（`deviceapi.Bridge.Interrupt`/`SubmitVoiceTurn`）只发一个 ESC，然后把下一句
`transcript + \r` 直接注入——结果注进了重定向框，造成：
- 内容**串轮**：上一轮残留 + 新一句混在一起；
- 偶发垃圾：`Unknown command: …`、把口语当指令；
- **状态错位**：固件日志 `turn_cancel_sent → keep committed turn(ADR-040) → topbar '草稿'`，
  以为"取消了"；claude 实际停在"等你重定向"。两边状态不是一回事 → 用户问的
  **"就绪可靠吗"，答案是：现在不可靠**，固件是本地乐观渲染，没等后台确认。

adapter 的 `extract/boundary` 只识别 spinner("esc to interrupt") 和 idle 提示符，
**不识别 "Interrupted · What should Claude do instead?" 这个重定向态** → 误判。

## 2. 设计目标（用户三诉求）

1. **撤销 = 干净中止 + 全新重发**：撤销后丢弃当前回合，claude 回到**真·空闲提示符**，
   下一句是**全新一轮**，绝不接在被撤销内容后面/被当成重定向。
2. **撤销的回合要被标记**：成功撤销的回合在设备 transcript + 会话记录里标「已撤销」
   （可见但不污染活动上下文）。
3. **状态要可靠（权威）**：固件的 就绪/忙/已撤销 反映 adapter 确认的 **claude 真实状态**，
   不是本地乐观猜测。

## 3. 架构总览：adapter 是回合状态的唯一真相源

```
   设备(固件)            云(中继)               adapter_v2 (PTY 真相源)
  乐观本地态  ──PTT/cancel──▶  透传  ──▶  控制 claude PTY + 抓屏判真实态
      ▲                                         │
      └──────── 权威 turn.* 事件 ◀── 透传 ◀──────┘  (committed/replying/replied/
                                                     cancelled/idle)
```

三层职责硬切：
- **adapter**：唯一懂 claude 真实态、唯一能干净中止、唯一发权威 turn.* 状态。
- **云**：纯中继，透传 turn.* 与 cancel，不做自己的回合状态逻辑。
- **固件**：本地态只做**即时乐观反馈**；**最终一切以 adapter 的 turn.* 为准**。

## 4. adapter：权威回合状态机 + 干净取消

### 4.1 识别 claude 的真实态（extract/boundary 扩一态）

| 态 | 屏幕特征 | 含义 |
|----|---------|------|
| `IDLE` | 空 `❯` 提示符,无 spinner,无 Interrupted 横幅 | 真·空闲(=就绪) |
| `BUSY` | spinner「✻ … esc to interrupt」 | 正在生成 |
| **`INTERRUPTED`** | **「Interrupted · What should Claude do instead?」+ ❯** | **ESC 后的重定向态(新识别)** |
| `PROMPT` | 工具权限/确认菜单(ADR-033) | 等设备审批 |

**关键新增：识别 `INTERRUPTED`。** 没有它,adapter 就会把"等重定向"误判成 IDLE 或 BUSY。

### 4.2 干净取消（turn.cancel / 纯撤销，不带新一轮）

```
1. 发 ESC 打断生成。
2. 轮询屏幕:若进入 INTERRUPTED("What should Claude do instead?"),
   再发一次 ESC(或经验证的"丢弃重定向"键)清掉它 → 回到 IDLE。
3. 确认屏幕到达 IDLE(空 ❯)后,才认定"取消完成"。
4. 发权威事件:turn.cancelled{seq} + turn.idle。
   被撤销回合记为 cancelled(不进入活动上下文)。
```
> 实现注意：第 2 步"哪个键丢弃重定向"需在真 claude 上验证（很可能是再按一次 ESC,
> 或 ESC 后输入框为空时回车）；这是本 ADR 最脆的一处,必须配 fixture 回归(同 ADR-033)。

### 4.3 干净 barge-in（撤销 + 紧接新一轮）

```
1. 先走 4.2 的"干净取消到 IDLE"(确认 claude 回到空 ❯)。
2. 到 IDLE 后,才注入新一句 transcript + \r → 保证是**全新一轮**,
   不接在被撤销内容后、不进重定向框。
3. 发 turn.cancelled{旧seq} + turn.committed{新seq, text}。
```
这一条直接实现诉求 #1（全新重发）。**当前 `SubmitVoiceTurn` 的"ESC 后立即注入"必须改成
"先确认回到 IDLE 再注入"。**

### 4.4 权威 turn.* 事件（扩 ADR-040）

| kind | 何时发 | payload | 设备语义 |
|------|--------|---------|----------|
| `turn.committed` | 注入成功(已在 ADR-040) | `{seq,turnId,text}` | 这句已作为一轮提交 |
| `turn.replying` | 检测到生成开始 | `{seq}` | 后台真在跑(↔ 设备显示"思考中") |
| `turn.replied` | 边界判定一轮结束 | `{seq,text}` | 权威回复文本 |
| **`turn.cancelled`** | 干净取消完成 | `{seq,reason}` | 该回合**已撤销**→设备标记+不污染上下文 |
| **`turn.idle`** | 屏幕确认回到空 ❯ | `{seq?}` | **claude 真空闲**→设备才显示"就绪"(可靠的关键) |

`turn.idle` 是"就绪可靠"的命门：**固件只有收到 turn.idle 才显示就绪**，否则显示"取消中…/思考中"。

## 5. 固件：乐观即时反馈 + 权威对账（UI 态机）

| 设备态 | 触发 | 来源 |
|--------|------|------|
| `LISTENING` | PTT 按下录音 | 本地乐观(即时) |
| `THINKING/思考中` | PTT 松开进 cloud_wait | 本地乐观 → 收 turn.replying 确认 |
| **`取消中…`** | 用户撤销/barge-in 后 | 本地乐观——**此时绝不显示"就绪"** |
| **`就绪`** | 收到 **turn.idle** | **权威**(adapter 确认 claude 空闲) |
| **`已撤销`(标记) | 收到 **turn.cancelled** | **权威**——把该回合气泡标删除线/「已撤销」灰显 |
| `草稿` | 本地捕获但还没 committed | 本地(过渡态;committed 到了就转正,cancelled 到了就转「已撤销」) |

要点：
- **就绪改为权威**：不再 PTT 松手就本地显示就绪;等 `turn.idle`。这修掉"就绪不可靠"。
- **撤销=标记而非静默保留/静默删**：收 `turn.cancelled` → 该回合标「已撤销」(灰显/删除线),
  留作可见记录,但它**不进入下一轮上下文**(上下文干净由 adapter §4.3 保证)。
  → 这把 ADR-040 的"撤销静默保留"升级为"撤销标记"。
- **草稿**是过渡态:committed 前显示草稿;`turn.committed` 到→转正常;`turn.cancelled` 到→转已撤销。

## 6. 与既有 ADR 的关系

- **修订 ADR-028 §2.5.1**：barge-in 从"本地 ESC + 静默撤回/保留"改为"adapter 干净中止到 IDLE +
  权威 turn.cancelled"。
- **扩展 ADR-040**：保留 turn.committed/superseded;新增 turn.replying/replied/cancelled/idle;
  固件"草稿"语义从"撤销保留"改为"committed 前过渡 + cancelled 后标记"。
- **依赖 ADR-035 抓屏**：INTERRUPTED 态识别 + IDLE 确认都靠 extract/vtscreen。

## 6.5 实现修正（来自对抗验证 2026-06-27，已核对代码）

三条经对抗验证后必须修订的实现约束：

1. **干净取消的状态机必须跑在 Run 的 goroutine 里，不能在 `Interrupt()`/`SubmitVoiceTurn` 里同步轮询。**
   `b.screen`(vtscreen) 是**单属主**(只有 Run 的 goroutine 喂它、读它),不是 goroutine-safe;
   而 `Interrupt`/`SubmitVoiceTurn` 跑在**传输 goroutine**。在那里同步轮询 `b.screen.VisibleText()`
   会与 Run 的 `b.screen.Feed` 数据竞争。**做法:加 `cancelReq chan cancelOp`(cap 1),
   `Interrupt`/barge-in 分支只往 channel 投请求;Run 的 loop 收到后在自己 goroutine 里执行
   "ESC → 轮询到 INTERRUPTED → 发 dismiss 键 → 轮询到干净 IDLE →(若 barge-in)注入新句 →
   发 turn.cancelled/idle"。`cancelOp{inject string}`:空=纯取消,非空=barge-in 要注入的文本。**
2. **双路径在 adapter_v2 + cloud_saas 下其实是死路。** adapter_v2 的 cloudrelay **没有
   `agent.message` 处理器**(proxy.go 只有复数 `agent.messages` 列历史;cloudrelay.go:343
   未知 kind 直接丢弃)。固件 `/v1/agent/message` fallback(bb_radio_app.c:2974 reply_text 空分支)
   在 cloud_saas 下会 hang/timeout,**根本到不了 Bridge**。所以**没有第二个活入口、没有状态漏洞**;
   turn.* 只在 WS voice 路径发即可。固件那条 fallback 是 **P4 清理**(P1/P3 不碰)。
3. **检测缺口已在代码确认**:`boundary.go:220 isIdlePromptLine()` 只认裸 `❯`/`>`,而 INTERRUPTED
   重定向屏也有裸 `❯` → `scanStatus` 报 idlePrompt=true → 因本轮已 `sawSpinner`,`TurnEnded()` 在
   重定向态就返回 true。修法:`extract/prompt.go` 加 `PromptInterrupted`;`Detector` 加
   `interruptedOnScreen` 字段,`refresh()` 里置位,`TurnEnded()` 在 sawSpinner 短路**之前**加
   `if d.interruptedOnScreen { return false }`。
4. **dismiss 键是唯一最脆假设**:集中成一个常量 `redirectDismissKey`(暂=第二个 ESC),真 claude 上
   验证后一处即可改。配 PTY fixture 回归。

## 7. 落地分期

- **P1（adapter，最关键）**：extract 识别 INTERRUPTED；`Interrupt()`/barge-in 改为
  "干净取消到 IDLE 再注入"；发 turn.cancelled + turn.idle。配 fixture 回归。
  → 单这一层就修掉"串轮 + 全新重发"两大病根（可在不烧固件下用 PTY 探针验证）。
- **P2（云）**：转发新 kind（turn.replying/replied/cancelled/idle）。
- **P3（固件）**：就绪改权威（等 turn.idle）；turn.cancelled → 标「已撤销」；草稿语义调整；
  「取消中…」过渡态。需烧固件 + 截图验证。
- **P4**：清理双路径历史包袱（见 §8）。

## 8. 待定 / 待查（需决策或真机验证）

1. **丢弃重定向的确切按键**（§4.2 第 2 步）——第二个 ESC？还是别的？真 claude 上验。
2. **双路径**：实测日志里设备既走 WS voice.stream，又在某些 turn 走
   `/v1/agent/message`（HTTP，带 sid）。这套 cloud_saas 下到底应只走一条？需厘清，
   否则状态机有两个入口、难保证一致。
3. **barge-in 手势细化**：纯撤销(不带新句) vs 撤销+新句;grace 窗(实测 2500ms)是否保留。
4. **`turn.idle` 的延迟**：干净取消要轮询屏幕到 IDLE 才发 idle，会有几百 ms;固件"取消中…"
   过渡态覆盖这段。可接受。

## 9. 验收

- 撤销后再说新句 → claude **全新一轮**，无上一句残留、无 redirect 串轮（§4.3）。
- 撤销的回合 → 设备标「已撤销」灰显（§5）。
- 撤销/取消未完成期间设备显示「取消中…」，**只有 claude 真回 IDLE 才显示「就绪」**（§4.4 turn.idle）。
- PTY fixture 回归覆盖 INTERRUPTED 识别 + 干净取消序列（防 claude 改 UI 失效）。
