# adapter_v2 阻塞式交互弹窗 → 设备确认（DESIGN.md §9 泛化）

> 文档位置：`design/adapter_v2_blocking_prompt_confirm.md`，配套新增 `design/decisions/ADR-033-adapter-v2-blocking-prompt-device-confirm.md`。
> 本文是 `adapter_v2/DESIGN.md` **§9（Phase 3 — tool 审批 scrape，issue #213）** 的正式扩写与泛化：把"仅 tool 审批"扩展为"**所有阻塞式 claude TUI 弹窗**"。§9 列的地基（`vtscreen.VisibleText()` / `extract/noise.go` 分类器 / `session.Write` / `extract/boundary.go`）全部沿用；本文在其上补全分类、状态机、协议、设备 UX，并修正 §9 一处关键勘误。

---

## 0. 关键勘误（正确性前置，必须先记下）

§9 L166 写道"等待审批态…已天然契合（spinner 在、idle 提示符未回时 `TurnEnded=false`）"。**核对 `extract/boundary.go` 后此论断不成立**：

- `TurnEnded`（boundary.go:122-144）的判定顺序是：settle 窗口过（L126）→ `spinnerOnScreen` 为假（L129）→ **`sawSpinner` 为真即 L137-139 直接 `return true`**，根本走不到 L143 的 idle-prompt 兜底。
- claude 渲染权限弹窗时 **spinner（"esc to interrupt"，`noise.go:28`）已被清掉**（claude 在等用户，不在等模型），而 `sawSpinner`（boundary.go:76-80）早在本轮已置真。
- 结果：**当前代码会在弹窗出现时误判 turn 结束**，触发 `maybeSpeak`（deviceapi.go:647），对设备播一段空白/上一轮脏文本（stale-seed guard 只在 `text==seed` 时拦，弹窗文本未必等于 seed）。

**因此"显式建模等待态"不是可选项，而是本特性的正确性前置**：必须与识别器同批落地。修复点是在 boundary.go:137 的 `sawSpinner` 短路**之前**插入 `if d.awaitingPromptOnScreen { return false }`（见 §3.2）。

---

## 1. 目标与范围

- **目标**：当 claude 交互式 TUI **阻塞在需要人来选的弹窗**时，adapter 把 `{问题, 选项[]}` 转发给设备（语音播报 + 屏幕/按键 + 语音选择），用户在设备端确认，选择注入回 PTY；并修正 boundary 不把"等待选择"误判为 turn 结束。
- **范围内**：claude 编号式弹窗（`Do you want to proceed? 1.Yes 2.No 3.…`）、编辑/工具确认、首启 fullscreen-renderer upsell、trust-folder、onboarding/theme。
- **范围外**：嵌入 Agent SDK / 改用 `-p` headless（DESIGN.md §1.1/§2 已否决，会丢 PTY 接管/resume）；firmware 上具体 LVGL 像素布局（仅定义协议契约 + 复用既有 page 模式）。
- **设计文档优先**：本特性触碰跨组件协议（新 envelope kind / 新 request kind），按 CLAUDE.md「Cross-Component Protocol Sync」必须先定契约再实现。

---

## 2. 弹窗分类（taxonomy）—— 谁靠配置压制、谁拦截转发

| 类 | 弹窗 | 字面锚点（匹配用） | 处置 |
|---|---|---|---|
| 1 | 首启 fullscreen-renderer upsell | `Try the new fullscreen renderer?` | **配置压制**：`CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=1` + 预置 `~/.claude.json` 的 `fullscreenUpsellSeenCount`。压不掉 → `claudeStartupKeys`（butler/session.go:106）auto-Enter 兜底（保留）。|
| 5 | trust-this-folder | `trust this folder` / `trust the files` | **配置压制**：持久化 per-project trust；现状 `warmup`（deviceapi.go:538-543）auto-Enter 兜底（保留）。|
| 6 | theme / onboarding | `Let's get started` / `Choose the text style…` | **配置压制**：预置 `hasCompletedOnboarding=true`/`theme`/`lastOnboardingVersion`；auto-Enter 兜底。生产 workspace 应预 onboard，永不阻塞。|
| 4 | rating / feedback survey | `How is Claude doing this session`（deviceapi.go:492 `surveyMarker`） | **保持现状 `dismissSurvey` auto-dismiss（注入 `0`，deviceapi.go:407-419）**。**显式排除出 forward-to-device 分类器**，绝不双重处理（见下）。|
| 2 | 非 bypass 权限请求 | `Do you want to proceed?` + `Yes, and don't ask again…` / `No, and tell Claude what to do differently` | **拦截转发（本特性主目标）**：仅当 skip-permissions 关闭时才渲染。|
| 3 | 工具/编辑确认（2 的子型） | `Do you want to make this edit` / `Do you want to create …` | **拦截转发**：同 2 的渲染前提。|

**survey（类 4）矛盾的最终裁决**：survey 既被现有代码证明是**输入阻塞模态**（deviceapi.go:485-489 注释明确"wedged the line for minutes"），又有稳定锚点 `How is Claude doing this session`——它**确实是阻塞模态**，但**已有专责处置者 `dismissSurvey`**。最终决策：**`dismissSurvey` 维持原样不动；新识别器 `ParsePrompt` 的 `PromptKind` 永不返回 survey，不进 forward 路径**。避免新识别器与 `dismissSurvey` 抢同一屏覆盖层。

**核心区分**：类 1/4/5/6 用**配置压制 / 既有 auto-handler**（比 scrape 稳，UI 改版不影响）；类 2/3 是真正的 **MUST-INTERCEPT**，是把 §9 从 tool-approval 泛化到"所有阻塞弹窗"的实质目标。

**匹配稳定性排序（最稳→最脆）**：选项动词文案（`Yes, and don't ask again for…` / `No, and tell Claude what to do differently`）> 问题头（`Do you want to proceed?`，claude 会改写）> 框线几何 / `❯` 高亮位置。**识别器一律按选项 label 子串锚定，绝不按框位置或硬编码数字位置。**

---

## 3. 检测机制 + 显式 "awaiting-prompt" 会话态

### 3.1 识别器（新文件 `internal/extract/prompt.go`）

仿 `toolstep.go:parseToolStep` 的"严格锚定 + 字段抽取 + rune 安全截断"结构：

```
type PromptKind   // permission | editConfirm | upsell | trust | unknown （无 survey）
type Option struct { Key, Label string; Default bool }
type Prompt struct {
    Kind       PromptKind
    Question   string
    Options    []Option
    Mechanism  string // "digit" | "arrowsEnter" —— 由 §11 P0 spike 决定
    Signature  string // 选项 label 串拼成的稳定指纹，用于 promptId 复用/supersede 判定
}
func ParsePrompt(visible string) (Prompt, bool)
```

- 复用 `noise.go` 的 `isBoxDrawingOnly`（noise.go:142）界定弹窗框、`isSpinnerLine`（noise.go:127）的**缺席**作为门控；新增 `isOptionLine`（正则 `^\s*[❯›>]?\s*(\d+)[.)]\s+(\S.*)$`）也落在 `noise.go`，使 claude UI 改版一处修。
- **复合判据**：一个 box 区域内 ≥2 条编号选项行 **且** 无 spinner = blocked-on-menu。
- **`❯` 歧义消解**：`❯` 同时是 claude 的 idle prompt glyph（`noise.go:111` 把 `❯` 当 prompt chrome）。识别器必须区分"菜单指针 `❯`（行内 `❯` 后跟编号选项）"与"idle 提示符 `❯`（独占一行的空提示符，`isIdlePromptLine` boundary.go:193）"。这要专门的 fixture（§10）。
- **default 选项识别**：MVP 先读 `❯`/`›` 指针字形（`VisibleText` 可见）。**注意 `VisibleText`（vtscreen.go:428-445）只写 `glyphRune`，丢弃所有 SGR/reverse 属性**，反白默认项对它不可见。若 §11 spike 判定必须箭头导航，则需给 vtscreen 加 `HighlightedRow() int`（读 `sgrOf` 的 `attrReverse`，vtscreen.go:512），见 §3.3。

### 3.2 boundary 改造（修正 §0 勘误）

`internal/extract/boundary.go`：
- `scanStatus`（L165-182）增加返回 `awaitingPrompt bool`（`ParsePrompt` 命中即真）。
- `Detector` 增字段 `awaitingPromptOnScreen`；`refresh`（L100-110）写入；`Reset`（L150）清。
- `TurnEnded`（L122）在 **L137 `sawSpinner` 短路之前**插入：`if d.awaitingPromptOnScreen { return false }`。
- 弹窗被选择注入后框消失，`refresh` 自然复位该位。

### 3.3 vtscreen（按需，由 spike 门控）

仅当 §11 spike 判定 claude 菜单**只接受箭头+Enter**（不接受数字直提）时，给 `vtscreen` 暴露 `HighlightedRow() int`（读 `g.Mode&attrReverse`，`sgrOf` 已具备能力，vtscreen.go:512）。digit-submit 可行则不需要，MVP 靠指针字形。

---

## 4. 数据模型

```
PromptSpec {
  promptId   string     // 单调递增 + 内容指纹（Signature）；选择回包据此对齐
  turnId     string     // 归属当前在飞 turn（不另起 turn 身份）
  kind       string     // permission | editConfirm | upsell | …（无 survey）
  question   string
  options:  [ { key, label, default } ]   // key = 字面注入键（"1"/"2"/…）
  selectionMechanism string              // "digit" | "arrowsEnter"
}
```

**注入契约（关键决策）**：option 携带**字面注入键**（仿 ADR-019 的 action 对象），firmware/cloud 全程 transport-dumb，只把用户选中的 `key` 原样回传，**adapter 负责把 `key` 翻成 PTY 字节**。数字 vs 箭头机制差异、claude 选项重排，都收敛在 adapter 一侧。

**promptId 生命周期与 supersede**：识别器每次 paint re-fire。若同一屏 box 内选项**指纹（Signature）变化**（claude 追加上下文相关选项 / 选项重排），adapter **supersede 旧 promptId**：向下游发 `PromptClosed{promptId, reason:"superseded"}` + 新 `PromptOpen`，并**拒绝针对旧 promptId 的 select**（ack-and-drop）。设备拿到新 PromptOpen 替换显示。

---

## 5. 单一权威「prompt-pending」门 + 协调所有既有 auto-injector（最大功能缺口）

**现状有三条独立注入路径会抢答正在转发的弹窗**：

| 注入器 | 位置 | 触发 | 风险 |
|---|---|---|---|
| `warmup` auto-Enter | deviceapi.go:538-543 | `isTrustPrompt` 命中 | trust 类自动 Enter（保留，类 5） |
| `dismissSurvey` 注入 `0` | deviceapi.go:407-419（每 Run loop 调） | `surveyMarker` 在屏 | survey 自动 dismiss（保留，类 4） |
| `claudeStartupKeys` 4×Enter | butler/session.go:106-119（**每次 spawn/respawn**） | 固定错峰 1.8/3.3/4.8/6.3s | **会落在任意 pending 弹窗上按掉默认项** |

**决策：引入单一权威 `promptPending` 状态**（Bridge 上一个 `atomic.Pointer[PromptSpec]` 或 `atomic.Bool` + 受锁 spec），**三条注入器全部先咨询它，pending 时 no-op**：
- `claudeStartupKeys`：本质是 spawn 后定时器，无法在 butler 层咨询 Bridge。**改为**：startup Enter 仅用于压制类 1/6（upsell/onboarding），且当 `ConfirmOnDevice` 开启时，由 Bridge 在 `warmup` 阶段接管启动序列、startup-keys 缩减或关闭；或在 ptyhost 注入前由 Bridge 设置的 gate 回调短路。**这是 P0 必须设计死的点**，否则 respawn 后的错峰 Enter 会替用户答掉新弹窗。
- `warmup` / `dismissSurvey`：仍处理各自的类 5/类 4，但**两者都不触碰类 2/3 权限弹窗**（识别器 kind 已区分），天然不冲突；额外加 pending 守卫做纵深防御。

**`maybeSpeak` / `onProgress` 抑制**：Bridge.Run（deviceapi.go:437-483）的 select 循环里，**pending 期间必须抑制 `maybeSpeak`（不播 TTS）、不 re-baseline（rebaseline 会 reset Detector）、不 emit `TurnIdle`**。这是 §0 修复（boundary 不 fire）之外的第二道闸：即便 boundary 因某种边界条件返回 true，pending 门也拦住播报。这触碰 package 内最精细的并发（`inFlight` atomic + rearm + maybeSpeak），是实现重点。

---

## 6. respawn / --resume 使 pending 弹窗失效（必须的协议规则）

`DeviceSession.New()` / `Resume()`（butler/session.go:131-155）respawn PTY 并重挂 `claudeStartupKeys`。若设备上有 pending 弹窗时发生 respawn（用户切对话 / 新对话），旧 promptId 指向已死 PTY，设备稍后的 `prompt.select` 会注入进**全新进程的输入框**（错上下文）。

**规则（写入协议）**：
- `respawn()`（butler/session.go:160）**作废所有 pending promptId**：向下游 emit `PromptClosed{reason:"respawn"}`。Bridge 的 cloud-relay dead-session 重建、web 重连本就发生，pending 态随 Bridge 重建清零。
- **firmware/cloud 必须把针对未知/已失效 promptId 的 select 当作安全 no-op（ack-and-drop），不报错**。

---

## 7. 协议：设备 ↔ adapter ↔ cloud ↔ firmware

复用 v1 既有词汇（ADR-030 display-only frame + 选择回包风格），**新增一个下行事件 + 一个上行请求**，全部 additive、ignore-unknown，按 `hello.proto`/menuVersion 门控（半灰度不破设备）。

**关键差异：`prompt.open` 不是 display-only。** `ToolStep`（transcript.go:375）是纯下行展示，无回路；`prompt.open` **需要 `prompt.select` 回路**，是全新的"设备→adapter 中途控制消息"（现有上行仅 `ptt.start/stop`、`turn.cancel`）。这是真正的新上行 envelope kind，cloud hub 必须把它路由为对 live home_adapter 的 request。

**adapter 侧统一插点 = `deviceapi.Events`**（deviceapi.go:133-152，与 ReplyDelta/ToolStep/TurnIdle 同构）：
- 新增 `PromptOpen(p PromptSpec)`、`PromptClosed(promptId, reason string)`（默认 no-op，向后兼容 nil Events）。
- 新增 `Bridge.SelectPromptOption(promptId, key string)`：校验 promptId 仍是当前 pending → `session.Write([]byte(key))`（机制可能 `+ "\r"`），复用 `interruptSettle`（deviceapi.go:95）时序，**绝不前置 ESC**（ESC 会取消弹窗）；注入后清 pending、re-baseline、emit `PromptClosed{reason:"answered"}`。

| 改动点（本仓库） | 同步校验（CLAUDE.md 表） |
|---|---|
| `internal/devicews/frames.go`：新增下行 `promptOpen{t,turnId,promptId,kind,question,options:[{key,label,default}]}`（仿 `toolStep` frames.go:155 / `replyEnd` :163）；`ctrlIn`（:115）增 `promptId/optionKey` 字段 | — |
| `internal/devicews/server.go`：`deviceConn` 增 `PromptOpen/PromptClosed` 实现 writeCtrl（仿 `ToolStep` :238 / `ReplyComplete` :231）；inbound switch（:348-366）增 `prompt.select` case → `bridge.SelectPromptOption`；`deviceConn` 增 pending-choice 态 | WebSocket envelope 格式 → Cloud hub 路由处理新 kind |
| `internal/cloudrelay/transcript.go`：`cloudEvents` 增 `PromptOpen/PromptClosed`（仿 `ToolStep` :375 的 active/write 守卫 + end() 后丢弃） | Notification/事件转发 → Cloud hub forwarding + Firmware WS handler |
| `internal/cloudrelay/cloudrelay.go`：`handleRequest`（:255）增 **独立** `prompt.select` request kind（见 §8 决策）→ 定位 live bridge → `SelectPromptOption` → ack | 新 agent proxy request kind → Cloud `handleRequest` dispatch + route registration |
| `internal/cloudrelay/proxy.go`：`driverCaps()`（proxy.go:148-150）`toolApproval` 由 `false` 翻 `true`，**按 `hello.proto`/menuVersion 协商 gated**（见 §9） | Session/能力上报 → Cloud agent proxy |
| `firmware/include/bb_adapter_client.h`：finish-stream-event 枚举增 `BB_FINISH_STREAM_EVENT_PROMPT`（+ 选项 payload / JSON blob） | Firmware session client |
| `firmware/src/bb_bbwire2.c`：`bw_handle_text`（kind 分发实际在 .c，**ADR 落 touch-point 前以 .c 的 `bw_handle_text` 为准，勿照搬草稿引的 .h:93-95**）增 `prompt.open` 解析；新增 `prompt.select` 上行发送器（仿 `ptt.start`） | bbwire/2 契约扩展 |
| `firmware/src/bb_radio_app.c` + 新 `bb_page_prompt_select.{c,h}`：仿 `bb_page_ota_confirm`（顶层 consent 页 + 倒计时 + accept/skip 回调，nav 路由拦 OK/BACK） | Firmware WS handler |
| `references/bbclaw-reference/cloud`：hub 转发 `prompt.open`（home_adapter→device）、接收 `prompt.select`（device→home_adapter）路由为 **parked request**（见 §8） | Cloud relay proxy |
| `adapter_v2/docs/device-protocol.md`：补 `prompt.open`/`prompt.select` schema，按 `hello.proto` 门控 | — |

---

## 8. Cloud parked-turn 生命周期（最关键的缺失同步点）

**问题**：`handleTranscript`（transcript.go:18）把一轮语音绑到 `begin()`/`end()`，硬上限 `ReplyWait=90s`（cloudrelay.go:85），且 `turnCtx`（transcript.go:55-90）**被任何更新的 transcript 抢占**（superseded 路径 L84-90）。一个 mid-turn 转发的弹窗若等人选择，会：(a) 受 90s think-time 限制；(b) 用户再说话 / PTT 误触 → 整轮被 ESC-abort，pending 弹窗被静默孤儿化。cloud relay **当前没有"此请求 parked 等待带外选择"的概念**。

**决策（最终）：`prompt.select` 走独立 request kind，且 parked turn 不可被 superseded。**

- `prompt.select` 在 cloud 注册为**独立路由**（不复用 voice.transcript 的 `handleTranscript`），把人的 think-time 与 `ReplyWait`、与 supersede-on-new-transcript 完全解耦。
- 弹窗 pending 期间，**承载它的原 turn 进入 parked 态**：cloud 不能因新 transcript 把它当 supersedable barge-in ESC 掉（否则会取消弹窗背后的工具）。需在 cloud 的 turn 注册表里给 parked turn 打标，supersede 逻辑（transcript.go:78）跳过 parked turn；或把弹窗背后的 turn 在 adapter 侧就保持 inFlight、不向 cloud 报 reply.end，直到 select 注入后弹窗消失、真正的 reply 才流出。
- **结论：此特性的 cloud 改动不能"P1 LAN-only 先发、cloud 后补"**——cloud relay 必须**先**学会 parked-turn 状态，否则远程设备会 hung-turn。LAN（devicews）路径无此 90s/supersede 约束，可先打通验证 UX，但 cloud 路径上线前 parked-turn 协议必须就位（见 §11 分期）。

---

## 9. 能力协商（capability negotiation，防半灰度 desync）

`driverCaps()`（proxy.go:148-150）把 `toolApproval` 翻 `true` 只是**生产者**侧。真正的同步点是**消费者**（cloud + firmware 是否会渲染 prompt UI）：一支半灰度的 fleet——adapter 报 `toolApproval:true` 但 cloud/firmware 不处理 `prompt.open`——会让设备 hung-turn 且无 UI。

**规则**：adapter **只有在协商确认对端 menuVersion/`hello.proto` 支持 prompt UI 后**，才允许进入 `forward-to-device`（即关 `--dangerously-skip-permissions`、让类 2/3 渲染）。协商失败 → 退回 `bypass`（默认行为），永不让一个不会答弹窗的对端面对一个会渲染弹窗的 claude。

---

## 10. 选择注入回 PTY

- **数字菜单**：`session.Write([]byte("1"))`（已被 `dismissSurvey` 注入 `0`，deviceapi.go:407-419 证明 claude 编号菜单按数字即提交）。
- **箭头菜单**：注入 `\x1b[B`/`\x1b[A` × N + `\r`。**本仓库无任何箭头转义注入先例**——`grep` 仅见 ESC（deviceapi.go:82,306,335）。需 §11 spike 真机抓包确认 claude 这些 Ink 菜单接受数字还是只接受箭头；若只接受箭头，则需 `HighlightedRow()`（§3.3）算出从当前高亮到目标的 N 次按键。
- **注入前后不前置 ESC**；落地后 adapter 主动 re-baseline extract、清 pending、emit `PromptClosed{answered}` + `TurnIdle`（若 turn 因此结束）。

---

## 11. 设备 / 语音 UX + 无设备降级不变量

- **TTS 播报**：`Synthesizer` 读问题 + 选项标签，如"Claude 想运行 rm -rf build。说 yes 允许，no 跳过。"
- **选择**：
  - **按键（鲁棒路径，安全确认默认走这条）**：6-button nav（ADR-015：UP/DOWN 移 selectedIndex、OK=确认高亮、BACK=拒绝/取消）。仿 `bb_page_ota_confirm` 顶层页。
  - **语音（便捷路径）**：pending 期间 PTT 说 "yes/no" 经 ASR，但**路由到 SelectPromptOption 而非 SubmitVoiceTurn**（否则会 ESC 打断并把 yes 打进弹窗）。安全敏感（rm/write）建议禁用语音或要求无歧义语法。
- **timeout / 默认（安全关键，写成不变量 + 测试）**：
  - 权限类（2/3）：超时 → **auto-DENY**（注入 No / ESC），绝不静默执行破坏性工具。
  - 装饰类（1/6）：超时 → auto-default（claude 自身默认 benign）。
  - 倒计时对齐 `bb_page_ota_confirm`；**adapter 侧也需独立超时**，避免未升级设备把 PTY 永久卡死。
- **无设备/桥断降级（显式不变量）**：当 `ConfirmOnDevice` 已配置但**当前无设备/桥接附着或设备 WS 断**时：
  - 类 2/3 → **auto-DENY**（绝不因没人听就 auto-approve 破坏性工具）。
  - 类 1/6 → auto-default。
  - 此不变量要有专门测试（无设备 + 权限弹窗 → 注入 No，断言不执行）。
- **`claudeStartupKeys` 互斥**：见 §5，`ConfirmOnDevice` 激活时不在 pending 期间触发默认 Enter。

---

## 12. 设置开关（settingsstore + butler）

`CLISettings` 现有 `SkipPermissions`（默认 true）/ `ClaudeAutoEnter`。改造：

- 权限处置改为**三态**（替代单一 `SkipPermissions` bool）：
  - `bypass`（默认，今天行为）：butler 追加 `--dangerously-skip-permissions`，类 2/3 永不渲染。
  - `forward-to-device`：**不追加**该 flag，类 2/3 渲染 → scrape → 转发设备 → 注入（**前提：§9 能力协商通过**）。
  - `blind-enter`：无设备部署，blind Enter 兜底。
- 新增 `PromptTimeoutSec` + 每类默认策略（deny vs default）。
- web admin 暴露上述开关。

**P0 实证探针（gate「P0 done」）**：butler 在受控、预 trust 的 workspace 跑（warmup 已确认 trust），**关掉 skip-permissions 后许多工具调用可能因预授权仍不弹窗**，只有少数（rm、网络）真弹。**必须一次实证：关 flag 跑真 claude，看哪些动作真弹**——否则该 headline 特性在默认部署里可能几乎不触发（feature 基本 inert）。结果写入 ADR-033。

---

## 13. 推荐方案（screen-scrape vs 结构化 API vs 混合）

**结论：混合（配置压制 + screen-scrape 为主），不嵌 SDK、不用 `-p`。**

1. Claude Code **不对外部进程暴露结构化权限 IPC**。`canUseTool` 回调只在 Agent SDK 库内（嵌入即丢 PTY/接管/resume，与 v2 立身之本冲突，DESIGN.md §1.1/§2）；hooks 是文件型 shell，非进程内事件；permission modes 只压制/自动批准、不外发事件。
2. 故 screen-scrape 是**今天唯一可行**路径——正是 §9 既定方向。
3. **混合优化**：能配置压制的（类 1/4/5/6）一律配置压制 / 既有 auto-handler，缩小 scrape 面；scrape 仅服务真正的类 2/3 决策弹窗。
4. 保留 v2 的 **TUI-mirror UX**：设备看抽取文本，手机/web 看真终端，弹窗对两路自然呈现。
5. 若未来 Claude Code 出结构化权限 IPC（socket/MCP），可在不丢 PTY 前提下平滑迁移。

---

## 14. 脆弱性护栏与 fixtures（硬发布门，非 nice-to-have）

`testdata/*.vt` 每个弹窗族一份，由 `gen_claude_fixture.go`（`//go:build ignore`，扩展 `promptBox`/`spinnerFrame`）合成。**新增 `prompt_test.go` 回放，happy-path + 对抗用例都是 release blocker**：

- (happy) `Do you want to proceed?` 权限框 → 断言 `{question,options}` 正确 **且** `TurnEnded` 保持 false。
- (a) **选项在同 box 下变化** → 断言 supersede（旧 promptId 关闭 + 新 open）。
- (b) **pending 期间 resize 重排**（web client 改 PTY 尺寸，`reconcileMirrorSize` deviceapi.go:560 触发 reflow）→ 断言 label 锚定仍正确解析。
- (c) **`❯` 作菜单指针 vs `❯` 作 idle 提示符** 消解。
- (d) **解析失败 fail-safe**：检测到弹窗框但 parse 不出 → 回退 auto-deny（权限类）/auto-default，**绝不挂起**。
- (e) **§0 回归**：`awaitingPrompt` 为真时 `TurnEnded` 必须为 false（即便 `sawSpinner` 真）。

匹配锚定选项 label 子串，不锚框位置/数字位置（§2）。

---

## 15. 分期落地（adapter-first）

**P0-spike（gating，先做，结果决定后续）**：
- **箭头 vs 数字** 真机抓包（claude Ink 菜单接受 digit-submit 还是只接受 arrows+Enter）→ 决定注入机制 + 是否需 `HighlightedRow()`。
- **skip-permissions-off 探针**：关 flag 跑真 claude，确认 butler workspace 里哪些动作真弹（特性是否可达）。
- 两个 spike 都廉价、都能否定大段设计；结论写入 ADR-033。

**P0（adapter-only，可独立打 tag）**：
1. `extract/prompt.go` + `noise.go` `isOptionLine` + boundary `awaitingPrompt` 修正（§0）+ 全套 fixtures（§14 含对抗用例）。
2. **单一 `promptPending` 门**（§5）+ 三注入器协调（warmup/dismissSurvey/claudeStartupKeys）+ maybeSpeak/rebaseline/TurnIdle pending 抑制。
3. `deviceapi.Events.PromptOpen/PromptClosed` + `Bridge.SelectPromptOption` + supersede + respawn 作废（§6）+ 无设备 auto-deny 不变量（§11）。
4. `settingsstore` 三态 + butler `ConfirmOnDevice` + `PromptTimeoutSec`。
5. 配置压制类 1/5/6（env + 预置 `~/.claude.json`）。

**P1（devicews LAN）**：frames + server `prompt.select` + pending-choice 态。设备物理近用户，先打通 LAN 验证 UX（无 90s/supersede 约束）。

**P2（cloud + firmware）**：cloud **parked-turn 状态 + 独立 `prompt.select` 路由（§8）** + 能力协商（§9）+ `bb_page_prompt_select` + bbwire/2 frame。需 firmware 协议同改 → **coordinated tag**。

**Tag 策略**：本文 + ADR-033 **docs-only 不打 tag**；P0/P1 改 adapter wire → **adapter 需 tag**；P2 引入新 firmware 协议 → **coordinated tag**（CLAUDE.md「One tag, one release」）。

---

## 16. ADR 交叉引用

- 新增 **ADR-033 — adapter_v2 阻塞弹窗设备确认**（仿 ADR-032 模板，含 related + phasing + 跨组件 touch-points 表 + 两个 spike 结论 + 安全不变量，CN）。
- 关联：ADR-019（server-driven menu rows/selectedIndex 词汇复用）、ADR-030（display-only frame 范式 + ignore-unknown 向后兼容；**注意 prompt 非 display-only，有回路**）、ADR-015（6-button nav）、`bb_page_ota_confirm`（顶层 consent 页先例）、issue #213。
- `CHANGELOG.md` 增 ADR-033 docs-only 条目。
