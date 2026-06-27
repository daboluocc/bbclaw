# ADR-040 — 设备 ↔ adapter 回合状态权威同步

- **状态**: Accepted（Layer-1 + Layer-2 三端均已实现并提交。adapter_v2 + 云已
  build + test 通过；固件待真机 flash 验证。云端改动已在私有 repo 提交，**未推送/未部署**）

## 实现落点（已提交）

| 组件 | 提交 | 关键文件 |
|------|------|----------|
| 固件 Layer-1（不撤回已提交回合 + 顶栏「未发送」） | `3b43ac8` / `b594afd`(+`8b160d8` 接线) | `bb_radio_app.c`、`bb_ui_agent_chat.c/.h` |
| adapter_v2 发 `turn.committed/superseded` | `015f2cf` | `cloudrelay/transcript.go`、`e2e_test.go` |
| 云转发两个 kind（WS + HTTP 两路） | `a4a9598`(私有 repo) | `cloud/internal/httpapi/server.go(_test)` |
| 固件消费对账 | `1072ff4` | `bb_adapter_client.c`、`bb_ui_agent_chat.c/.h`、`bb_chat_transcript.c/.h` |

实测：adapter_v2 `make e2e`（真 PTY）断言 `turn.committed{seq:1,text}` 先于 reply;
云 `TestStreamFinishStreamingForwardsTurnCommitted` 断言转发到设备帧。固件 5 文件改动
符号自洽,待 bench idf 会话释放后 build+flash 验证（未抢占共享 build）。
- **日期**: 2026-06-27
- **关联**: ADR-028（对话核心 v2 / barge-in 撤回语义，本 ADR 修订其 §2.5.1）、
  ADR-030（设备执行步骤）、ADR-033（阻塞菜单确认）、ADR-035（adapter_v2 交互式 PTY）

## 1. 背景（线上真实链路）

线上跑的是 **adapter_v2**（`bin/bbclaw-adapter-v2`），它把一个真 `claude`
拉进 PTY，抓屏抽取助手文本。cloud_saas 下三方链路是：

```
firmware  ──WS──▶  云中继(cloud relay)  ──Envelope──▶  adapter_v2(PTY/claude)
  设备屏 ◀── voice.reply / reply.delta / asr.final / tool_call ◀──┘
```

云端做 ASR/TTS。一轮语音：设备流音频 → 云 ASR 出 transcript → 云把 transcript
发给 adapter_v2 `cloudrelay.handleTranscript` → `SubmitVoiceTurn` 注入 PTY →
claude 回复 → adapter 抽取 → 回 `voice.reply` 给云 → 云 TTS 回设备。

### 1.1 复现的 bug（用户报告）

> PTT 释放"丢失"导致那句没正常收尾；**设备屏上看不到这句**，只有 adapter 的
> web 终端（xterm 直连 PTY）看得到。再按一次 PTT 说下一句，上一句才被"推"出去；
> 但固件只显示最新那句（"继续啊"），看不到上面那段内容。要有一个同步机制保证
> 设备与 adapter 状态一致。

### 1.2 根因（已逐行核对）

不是 GPIO 边沿真的丢，而是**设备本地单方面撤回了一个 adapter 已经提交的回合**：

1. 句①录完、PTT 释放 → `bb_radio_app.c` 进入 `!s_ptt_pressed && streaming`
   分支（`firmware/src/bb_radio_app.c:2750`），调用阻塞的
   `bb_adapter_stream_finish_stream()`（`:2863`），`s_cloud_wait_busy=1`。
   此时云已 ASR 完、已把句①注入 adapter/claude，**claude 已在生成**（web 终端可见）。
2. claude 回复慢（Orbiting/thinking）。设备仍卡在 cloud_wait。
3. 用户按第二次 PTT（句②"继续啊"）。超过 barge-in grace 窗口 →
   `on_ptt_changed` 判为 **barge_in**（`:986`）→ `bb_adapter_abort_finish_wait()`
   （`:1007`）本地中断句①的 finish 等待 + 发 `turn.cancel`。
4. 句①的 `finish_stream` 返回 `ABORTED_BY_USER` →
   `bb_radio_app.c:2958` 分支 → **`bb_ui_agent_chat_withdraw_last_turn()`**
   （`:2969`，ADR-028 §2.5.1 撤回语义）→ 把句①整轮（问题气泡 + 半截回复）
   从设备 transcript 删掉。
5. 但 adapter 侧句①**早已提交并被 claude 处理**（文本留在 PTY scrollback）。
   `turn.cancel` 只是 `cb.preempt()` + `Interrupt()`（ESC）打断生成，**不回滚已发生
   的回合**。于是 web 终端保留句①，设备删了句①。
6. 句②注入 → claude 回复 → 设备只显示"继续啊"。

**结论**：设备的回合生命周期由本地状态机单方面决定（barge-in 撤回、stale
`stream_id` 丢弃 reply），与 adapter 实际"已提交/已回复"的回合**没有任何对账通道**。
两边一旦判断不一致（本地撤回 vs adapter 已提交；或 finish 被 abort 而云端已 commit），
就永久发散，无自愈。**用户要的"同步机制"，本质就是补上这条缺失的对账通道。**

设备本地无法区分两个落在同一条 barge-in 路径上的手势：
- **「说错了，撤销」** → 应整轮撤回（ADR-028 现行行为）。
- **「边想边补一句 / 继续说」** → 上一轮是真的、adapter 已提交，**不该撤回**。
只有 adapter 知道这一轮到底有没有真正提交/回复 —— 所以权威判定必须来自 adapter。

## 2. 决策

分两层落地，互补：

### Layer 1 — 固件本地止血（无协议改动，已实现）

barge-in 的 `ABORTED_BY_USER` 收尾里，**只撤回 adapter 从未提交的回合**：以
"本轮是否已显示 `asr.final`（`ui_stream->transcript` 非空）"作为"adapter 已提交"
的本地代理信号。

- transcript **已显示** ⇒ 云已转写并已把它路由给 adapter（`cloudrelay` 会
  `SubmitVoiceTurn`）⇒ 这是真回合 ⇒ **保留用户问题气泡，不撤回**（只是它的回复被
  打断了而已）。
- transcript **为空** ⇒ ASR 出结果前就被打断 ⇒ 误触/空轮 ⇒ 维持原 **整轮撤回**。

代码：`firmware/src/bb_radio_app.c` `ABORTED_BY_USER` 分支（约 `:2958`）。

**这是对 ADR-028 §2.5.1 的修订**：撤回语义从"barge-in 一律撤回上一轮"收窄为
"只撤回未提交（无 asr.final）的上一轮"。代价：用户在 asr.final 已回之后才想"撤销"
那一轮，单击不再删除它（本地已无从区分撤销 vs 继续）；Layer-2 上线后由 adapter
权威信号彻底解决。

Layer-1 是启发式止血，**不改变两边可能发散的根本事实**，仅显著降低最常见的发散
（"补一句话"误删上一轮）。

### Layer 2 — adapter 权威回合同步（跨端，云端契约待确认）

让 **adapter_v2 成为回合状态的真相源**，发权威的、带单调序号的回合生命周期事件；
云中继转发；固件维护一份**权威回合日志**并用它对账本地乐观 UI。

#### 2.1 新增 envelope（提案，待云端确认）

adapter_v2 → 云 → 设备，均带 `seq`（每设备会话单调递增）与 `turnId`：

| kind | 触发 | payload | 设备语义 |
|------|------|---------|----------|
| `turn.committed` | `SubmitVoiceTurn` 注入成功后 | `{seq, turnId, text}` | 这句已被我作为一轮提交；设备**必须**显示该用户气泡（本地若已撤回则补回） |
| `turn.superseded` | `preempt()`/barge-in 取消在飞轮 | `{seq, turnId, reason}` | 该轮被新轮取代；设备据 `reason` 决定保留(committed)或删除(从未 commit) |
| `turn.reply` | `ReplyComplete`（即现有 `voice.reply`，补 `seq/turnId`） | `{seq, turnId, text}` | 该轮权威回复文本 |
| `turn.idle` | `TurnIdle` | `{seq, turnId}` | 该轮结束，可对账收尾 |

`turn.committed` 是关键新增：现状 `cloudrelay/transcript.go` 只回
`voice.reply.delta`/`tool_call`/`voice.reply`，**从不回"我提交了哪句"**，设备只能靠
云自己的 `asr.final` 猜。补上它，设备才能在本地状态机判断错误时被纠正。

#### 2.2 固件：权威回合日志 + 对账

- 维护 `seq → {turnId, text, state}` 的小环形日志（已有 transcript 缓存可扩展）。
- 收到 `turn.committed{seq,text}`：若本地无此 turn（曾被 barge-in 撤回）→ **补回**用户气泡。
- 收到 `turn.superseded{reason}`：reason=`never_committed` → 删除本地乐观气泡；
  reason=`interrupted_after_commit` → 保留，标注「已中断」。
- `seq` 出现缺口/乱序 → 记 WARN 并以 adapter 日志为准重绘最近 N 轮（轻量 resync）。
- 本地 PTT 驱动的 UI 降级为**乐观显示**：先本地画，最终一切以 adapter 的
  `turn.*` 为准。

#### 2.3 云中继：转发新 kind

`turn.committed/superseded/idle` 在云 hub 路由按现有 `voice.reply.*` 转发路径透传到
设备；`turn.reply` 即给现有 `voice.reply` 补 `seq/turnId` 字段（向后兼容，旧设备忽略）。
**云端在私有 repo（bbclaw-reference），此节改动需先确认再落地。**

## 3. 影响面 / 触点

| 改动 | 仓库/文件 | 风险 |
|------|-----------|------|
| Layer-1 撤回收窄 | `firmware/src/bb_radio_app.c`（ABORTED_BY_USER 分支） | 改 ADR-028 §2.5.1 行为，需回归 barge-in UX |
| `turn.committed` 等发送 | `adapter_v2/internal/cloudrelay/transcript.go`、`internal/deviceapi` | 新事件，加 seq/turnId |
| 转发新 kind | 云 hub（私有 repo）| 跨仓库契约，**待确认** |
| 设备对账 | `firmware/src/bb_adapter_client.c`（WS 帧解析）、`bb_ui_agent_chat.c`（补回/标注/resync） | 新增帧处理 + 回合日志 |

## 4. 取舍

- **为什么不纯固件解决**：本地无法区分"撤销"与"继续补话"，也不知道 adapter 是否
  已 commit。任何纯固件启发式都只是降低发散概率，不能保证一致。权威判定必须来自
  adapter，故 Layer-2 需要跨端。
- **为什么加 seq**：检测乱序/丢帧/重连后状态漂移，是"对账"而非"尽力转发"的前提。
- **向后兼容**：新字段/新 kind 旧设备/旧云忽略即可，可灰度。

## 5. 验收

- 复现场景：句①（asr.final 已回）→ 慢回复期间按 PTT 说句② →
  **设备保留句①、追加句②**，与 web 终端一致（Layer-1 即可达成）。
- 极端场景：finish 被 abort 但云端已 commit → adapter 发 `turn.committed` →
  设备补回句①（Layer-2）。
- barge-in「说错了撤销」（asr.final 未回即打断）仍整轮撤回（Layer-1 保留该行为）。
