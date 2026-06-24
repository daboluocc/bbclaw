# ADR-033: adapter_v2 阻塞式交互弹窗 → 设备确认（截屏识别 + 转发选择）

- **日期**: 2026-06-24
- **状态**: 进行中（设计定稿；两个 P0 探针**已真机验证 2026-06-24**：数字直提成立、需显式 `--permission-mode default` 才弹）。**P0（adapter 核心）+ P1（devicews LAN）+ P2-a（cloudrelay parked-turn）已实现并经多智能体对抗审查 + 修复**：识别器 `extract/prompt.go` + boundary §0 勘误 + deviceapi 转发/注入/超时 auto-DENY + settingsstore/butler 接线（opt-in `ConfirmOnDevice` 默认 off）+ devicews `prompt.open`/`prompt.select`/`prompt.close` 端到端回路 + cloudrelay `voice.prompt.*` 转发 + parked-turn（弹窗 pending 不超时不 supersede）+ `prompt.select` 独立 request kind + `toolApproval` 能力门控；`go test -race ./...` 全绿。**P2-c firmware（LAN bbwire/2）已实现**：`bb_adapter_client.h` 事件 + `bb_prompt_t`（#6 定结构化字段）、`bb_bbwire2.c` cJSON 解析 `prompt.open`/`close` + 发 `prompt.select`、新 `bb_page_prompt_select` LVGL 确认页（仿 ota_confirm，选中默认落拒绝项 §11、倒计时 auto-deny）、`bb_radio_app.c` 路由；`make build`(idf.py) 通过，sim `--mode prompt_select` 目视确认布局 + 安全默认。**P2-b cloud hub 转发已实现**（reference 仓 PR #38）：server.go 两条设备语音路转发 voice.prompt.open/close → 设备；prompt.select 经 RouteFromPeer 通用回路由 home_adapter（adapter handlePromptSelect P2-a 接）；router 端到端 + promptStreamType 单测，go build/test 全绿。**待做**：固件 cloud-client（解析 voice.prompt.* + 发 prompt.select，P2-c 已做 LAN bbwire2 路）；真机 flash + 端到端验证（需回家连 LAN）；P0-5 upsell/trust 配置压制（暂由 claudeStartupKeys/warmup 兜底）。
- **关联**:
  - 完整设计：[`design/adapter_v2_blocking_prompt_confirm.md`](../adapter_v2_blocking_prompt_confirm.md)（本 ADR 是其决策摘要）
  - `adapter_v2/DESIGN.md §9`（Phase 3 — tool 审批 scrape，issue #213）—— 本特性是它从「仅 tool 审批」到「**所有阻塞式弹窗**」的泛化
  - ADR-032（会话生命周期 —— respawn 作废 pending 弹窗复用其 PTY 重启路径）
  - ADR-030（display-only frame 范式 + ignore-unknown 向后兼容 —— **注意本特性非 display-only，带 `prompt.select` 回路**）
  - ADR-019（server-driven menu rows / selectedIndex 词汇复用）、ADR-015（6-button nav）、ADR-031（设备审批 UX fast-follow 诉求）

## 背景

adapter_v2 用一根常驻 PTY 跑交互式 `claude` TUI（弃 `claude -p`，保留接管 / resume / 双视图，DESIGN.md §1.1/§2）。当 claude **阻塞在需要人来选的弹窗**（首启 fullscreen-renderer upsell、打分、trust-folder、onboarding，以及关掉 `--dangerously-skip-permissions` 后的工具/权限确认 `Do you want to proceed? 1.Yes 2.No…`）时，瘦语音设备无从应答，整轮卡死。诉求：把 `{问题, 选项[]}` 转发到设备，用户在设备端确认，选择注入回 PTY。

调研另外暴露一个与本特性独立的正确性 bug（详见设计文档 §0）：`extract/boundary.go:137` 在 `sawSpinner` 为真时直接判 turn 结束；claude 渲染权限弹窗时 spinner 已清、`sawSpinner` 已为真 → **当前代码会把弹窗误判成 turn 结束，对设备播空白/上一轮脏文本**。修复（显式建模「等待选择」态）是本特性的正确性前置。

## 决策

### 1. 推荐方案：混合（配置压制 + screen-scrape 为主），不嵌 SDK / 不用 `-p`

经代码核对，Claude Code **不对外部进程暴露结构化权限 IPC**：`canUseTool` 只在 Agent SDK 库内（嵌入即丢 PTY 接管 / resume，与 v2 立身之本冲突）；hooks 是文件型 shell 非进程内事件；permission mode 只压制 / 自动批准、不外发事件。因此 **screen-scrape 是今天唯一可行路径**，正是 §9 既定方向。混合优化：

- **配置压制**（比 scrape 稳，UI 改版不受影响）：首启 upsell / 打分 survey / trust-folder / onboarding（弹窗分类 1/4/5/6）一律用 env + 预置 `~/.claude.json` gating keys，或现有 auto-handler（`dismissSurvey`、`warmup`、`claudeStartupKeys`）处置。
- **拦截转发**（本特性主目标）：仅真正需要人决策的权限 / 工具确认弹窗（分类 2/3）走识别 → 转发设备 → 注入。

未来若 Claude Code 提供结构化权限 IPC（socket / MCP），可在不丢 PTY 前提下平滑迁移。

### 2. 协议：新增 `prompt.open` 下行 + `prompt.select` 上行回路

与既有 display-only frame 不同，`prompt.open` 需要 `prompt.select` 回路（首个「设备 → adapter 中途控制消息」）。adapter 侧统一插点 = `deviceapi.Events`（`PromptOpen` / `PromptClosed` + `Bridge.SelectPromptOption`）。option 携带**字面注入键**，firmware/cloud 全程 transport-dumb，adapter 负责把 key 翻成 PTY 字节。跨组件同步点见下「跨组件协议同步」与设计文档 §7。

### 3. 安全不变量（已采纳 2026-06-24）

- **超时 / 无设备 / 桥断**：破坏性权限弹窗（分类 2/3）一律 **auto-DENY**（注入 No / ESC），绝不因没人听就 auto-approve；装饰类（1/6）才走 claude 自身 benign 默认。
- **破坏性动作（rm/write/联网）只走按键确认**，关闭语音 yes/no 路径，避免 ASR 歧义误批。
- **设备「OK」≠ 直接接受 claude 高亮的默认 Yes**：破坏性动作要求用户显式离开默认项再确认。
- 这三条写成代码不变量 + 专门测试（无设备 + 权限弹窗 → 注入 No，断言不执行）。

### 4. 单一权威 `promptPending` 门

现有三条注入器（`warmup` auto-Enter、`dismissSurvey` 注入 `0`、`claudeStartupKeys` 4×Enter）会抢答正在转发的弹窗。引入 Bridge 上单一权威 `promptPending` 态，三者 pending 时 no-op；pending 期间抑制 `maybeSpeak` / rebaseline / `TurnIdle`。`claudeStartupKeys`（之前为跳过首启 upsell 加的 auto-enter）转为**无设备兜底**并受此门协调。

### 5. respawn 作废 pending 弹窗

`DeviceSession.New/Resume` respawn PTY 时，旧 promptId 指向已死进程。`respawn()` 作废所有 pending promptId（emit `PromptClosed{reason:"respawn"}`）；下游对未知 / 失效 promptId 的 select 当安全 no-op（ack-and-drop）。

**已知 gap（P1，可接受）**：设备在弹窗 pending 时**断线**，`Bridge.Run` 随 ctx 取消退出 → `closePendingPrompt` 清 pending 但**不注入 deny**（超时 auto-deny 的计时器在已退出的 Run 里，对这台 bridge 不再生效）。由于是**共享 PTY**，菜单仍留在屏上：可由 web 终端用户应答，或下次设备重连/新 bridge 重新检测后 90s auto-deny 收掉。安全不变量仍成立（绝不 auto-approve，菜单只是 blocked）。未主动 deny 是为避免多视图下把别人正要应答的菜单从下面 deny 掉。

## 两个 gating 探针（已真机验证 2026-06-24，claude 2.1.186）

1. **注入机制 → 数字直提（digit-submit）成立。** 对权限菜单注入单个数字 `1`（**无 CR、无箭头**）即立即选中并提交该项，工具随即执行。因此注入机制 = `session.Write([]byte("<digit>"))`；**不需要箭头转义序列，也不需要给 vtscreen 加 `HighlightedRow()`**（设计 §3.3 取消，§10 走 digit 路径）。与既有 `dismissSurvey` 注入 `0` 一致。
2. **可达性 → 默认环境不弹，必须显式配置才可达。** 用户 `~/.claude/settings.json` 仅 `allow: Bash(ls:*)`，但**裸跑（无 flag）`echo` 仍自动执行不弹窗**——因持久化默认权限模式是 auto-accept（`~/.claude.json` 的 `autoPermissionsNotificationCount` / `hasSeenAutoModeEntryWarning` 等键）。只有显式 `--permission-mode default` + 非 allowlist 工具（`cat /etc/hostname`）才弹出真菜单：

   ```
   Bash command: cat /etc/hostname  /  Read system hostname file
   Do you want to proceed?
   ❯ 1. Yes
     2. Yes, allow reading from etc/ from this project   ← 上下文相关、文案/数量会变
     3. No
   Esc to cancel · Tab to amend · ctrl+e to explain
   ```

   **推论（设计修订）**：`forward-to-device` 模式必须**显式追加 `--permission-mode default`**——仅去掉 `--dangerously-skip-permissions` 不够，会被持久化的 auto-accept 模式吞掉。特性确认可达，但**默认部署几乎不触发**，必须显式配置（且部署需关 auto-accept）→ P1/P2 投入前以此为准。
   该弹窗 `❯` 高亮 option1（=Yes）、`Esc` 取消（印证「注入不前置 ESC」「设备 OK ≠ 默认 Yes」两条不变量），option2 文案随上下文变（印证「label 子串锚定 + signature 变即 supersede」）。

## 分期（adapter-first）

- **P0-spike**：上面两个探针（gating）。
- **P0（adapter-only，可单独 tag）**：`extract/prompt.go` 识别器 + `noise.go isOptionLine` + boundary 勘误修复 + 全套对抗 fixtures；`promptPending` 门 + 三注入器协调；`deviceapi` PromptOpen/Closed + SelectPromptOption + supersede + respawn 作废 + 无设备 auto-DENY 不变量；settingsstore 权限三态（bypass / forward-to-device / blind-enter）+ `PromptTimeoutSec`；配置压制类 1/5/6。
- **P1（devicews LAN）**：frames + server `prompt.select` + pending-choice 态。设备近用户，先打通 LAN 验证 UX（无 90s / supersede 约束）。
- **P2（cloud + firmware，coordinated tag）**：cloud parked-turn 状态 + 独立 `prompt.select` 路由（不复用 voice.transcript，与 `ReplyWait=90s` / supersede-on-new-transcript 解耦）+ 能力协商（防半灰度 desync）+ `bb_page_prompt_select`（仿 `bb_page_ota_confirm`）+ bbwire/2 frame。

**Tag 策略**：本 ADR + 设计文档 docs-only **不打 tag**；P0/P1 改 adapter wire → adapter 需 tag；P2 引入新 firmware 协议 → coordinated tag（CLAUDE.md「One tag, one release」）。

## 跨组件协议同步（CLAUDE.md「Cross-Component Protocol Sync」）

| 改动 | 同步校验 |
|---|---|
| devicews `prompt.open` 下行 / `prompt.select` 上行 | Cloud hub 路由处理新 envelope kind |
| cloudrelay `prompt.select` 独立 request kind + parked-turn | Cloud `handleRequest` dispatch + route registration；parked request 路由（reference 仓） |
| cloudrelay `cloudEvents.PromptOpen/Closed` | Cloud hub forwarding + Firmware WS handler |
| proxy `driverCaps.toolApproval` 翻 true（menuVersion 协商 gated） | Cloud agent proxy 能力上报 |
| firmware `bb_bbwire2.c` 解析 `prompt.open` + 发 `prompt.select`、新 `bb_page_prompt_select` | Firmware session client + WS handler |

**能力协商门控**：adapter 只有在协商确认对端 menuVersion / `hello.proto` 支持 prompt UI 后，才允许进入 `forward-to-device`（关 skip-permissions）；协商失败退回 `bypass`，永不让不会答弹窗的对端面对会渲染弹窗的 claude。

## 风险

- **脆弱性**：识别器键于 claude 当前 TUI 文案 / 框结构，claude 改审批 UI 即失效 → fixture 回归（含对抗用例）作为**硬发布门**，按选项 label 子串锚定（不锚框 / 数字位置），parse 失败一律 fail-safe-to-deny。
- **并发**：在 deviceapi 最精细的 `inFlight` + rearm + maybeSpeak 区插 `promptPending`，易引入 races / 漏播 / 重播 → 充分单测。
- **半灰度 desync**：adapter 报 `toolApproval:true` 但 cloud/firmware 未处理 `prompt.open` → 远程设备 hung-turn；靠能力协商门控缓解。
- **cloud 耦合**：弹窗 think-time 与 90s `ReplyWait` + supersede-on-new-transcript 冲突；cloud parked-turn 改动**不可后补**（远程上线前必须就位）。
- **可达性**：见探针 2 —— 默认部署里主路径可能几乎不触发，投入 P1/P2 前以探针结论 gate。

## 后果

- 修掉一个独立于本特性的正确性 bug（弹窗误判 turn 结束 → 播空白 / 脏音频）。
- 设备获得对工具 / 权限的细粒度确认能力（可选关掉 bypass）；安全敏感动作有硬不变量护栏。
- 引入对 claude TUI 文案的脆弱依赖，以 fixture 硬发布门 + fail-safe-to-deny 兜底。
