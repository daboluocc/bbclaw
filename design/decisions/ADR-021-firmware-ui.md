# ADR-021-firmware-ui: 管家模式固件 UI —— Task List / 派发状态注入 / 底部状态栏 / PTT 文案

- **日期**: 2026-06-03
- **状态**: 草案（docs-only；C/D/E 三个实现 PR 由本 ADR 解锁，独立排期）
- **关联**:
  - [ADR-021](ADR-021-conversational-orchestrator-butler.md)（对话式编排管家——事件源基础）
  - [ADR-022](ADR-022-memory-consolidation-and-profile-docs.md)（记忆整理——inbox/profile 两层；§1 memory stats API 对齐本 ADR）
  - [ADR-019](ADR-019-server-driven-menu-protocol.md)（server-driven 菜单协议——menu.SessionItem 扩展契约）
  - [ADR-012](ADR-012-fixed-page-menu.md)（固定三页菜单——footer 不违反 no-overlay 原则，见 §2）
  - Issue [#99](https://github.com/daboluocc/bbclaw/issues/99)（v2 修订 issue）
  - Issue [#97](https://github.com/daboluocc/bbclaw/issues/97)（v1 原始 issue）

## 修订日志

| 版本 | 日期 | 说明 |
|---|---|---|
| v1 | 2026-06-03 | 初稿（#97/#98），含 menu.SessionItem.Role、session picker、Sub-PR B/C/D/E |
| **v2** | **2026-06-03** | **移除 session 抽象暴露，改为 Task List 范式。删除 §1.1（menu Role 字段）、§3（PTT 长按二义性 / Settings worker toggle）、Sub-PR B；新增 §1.4（/v1/butler/dispatch/recent API）、§3'（Task List 页交互）、§4'（Task List 视觉规格）；修订 §1.2/§2/§4/§5/§6。§1.1/§3/Sub-PR B 在 v1 → v2 演进期间从未实现，删除无损。** |
| **v3** | **2026-07-03** | **新增 §9：精简主菜单 + 提醒页 + 顶栏 🔔 未读徽标（承接 ADR-042 §10 提醒归 adapter）。待机短按 OK 从「直进 SETTINGS」改为「进主菜单」（对话 / 提醒·N / 设置），设置整组降为主菜单子项；设备把 adapter 的两张表（提醒 store + 通知 outbox）呈现为**一个**「提醒」页（即将 / 已提醒两段），消除设备侧「通知 vs 提醒」重复。** |
| **v3.1** | **2026-07-04** | **实现修订（真机反馈后）：撤销独立「精简主菜单」——用户要求「统一只有一个设置」。待机短按 OK 仍**直进 SETTINGS**（回到 v2 行为）；「提醒」改为 **Settings 列表里的一行**（置顶，`提醒 · N`），点开即 §9.2 的已提醒页。§9.1 的主菜单作废。§9.3 未读感知保留但落在**待机屏的「提醒 N」徽标**（被动指示，非菜单）。根因修复：无会话提醒（sid 空）此前被 `session.notification` 分支丢弃、从不入 store（badge 恒 0、已提醒恒空），已改为 sid 或 preview 非空即入 store。** |

---

## 背景

ADR-021 v1（#78 合并，子 issue #84–#88 / #83 全部 merged）已在 **Adapter 层**落地对话式编排管家：

- `logicalsession.Role`（RoleButler / RoleWorker）已定义，`ListDeviceFacing` 已过滤 worker
- `butlermcp/server.go` + `cmd/mcp-server` 子命令已托管 `dispatch/list_projects/task_status/task_result` 工具
- `butler.Engine` 通过 `Deps.MemoryWriter` 在每个 butler turn 末写入 workspace `CLAUDE.md` 收件箱
- ADR-022（#91 merged）已落地 consolidation 引擎，`workspace/MEMORY/preferences.md` / `projects.md` / `decisions.md` 三层画像文档就绪

但**固件 UI 没有任何视觉/交互变化**——管家在工作，设备对用户是"隐形"的。

本 ADR v2 确立核心范式：**管家是用户唯一的对话伙伴。固件 UI 不再暴露 session 抽象**——所有 session 逻辑（butler routing / worker dispatch / multi-session）都是 adapter 内部细节，对设备透明。设备只关心"管家在干什么"和"派发任务进度"。

`EnsureButler`（`httpapi/agent.go:655`）已强制 device turn 路由到 butler，session picker 早已名存实亡；`ListDeviceFacing`（`manager.go:265`）已过滤 worker。v1 的 §1.1 menu Role 字段与 §3 Settings worker toggle 从未实现，v2 删除无损。

本 ADR 的任务是**把协议契约和架构决策落到纸面**，作为后续三个实现 PR（C/D/E）的单一设计源。

---

## §1 协议增量（C/D/E 的契约源头）

### 1.2 dispatch_status 事件——复用 tool_use / tool_result（→ Sub-PR C）

**关键架构决策**：不新建跨进程 IPC 通道。MCP server（`cmd/mcp-server`）是 claude CLI 起的 stdio 子进程，跨进程注入要搞 socket/file IPC，不必要。

**更好的路径**：claude CLI 本身就把 `mcp__bbclaw__dispatch` 工具调用以 stream-json 的 `tool_use` / `tool_result` 帧吐出来。`claudecode/driver.go` 当前行为：

- `:510` `case "tool_use"` → 发 `EvToolCall`（所有工具统一处理）
- `:528` `tool_result`（`user` envelope）→ **当前丢弃**

**adapter 侧新行为**（仅针对 `mcp__bbclaw__*` 前缀工具，其他保持不变）：

```go
// adapter/internal/agent/claudecode/driver.go
case "tool_use":
    if strings.HasPrefix(c.Name, "mcp__bbclaw__") {
        s.emit(agent.Event{
            Type:     agent.EvDispatchStatus,
            Dispatch: &agent.DispatchStatus{Phase: "started", TaskID: c.ID, Input: c.Input},
        })
    } else {
        // 现有 EvToolCall 逻辑，不变
        s.emit(agent.Event{Type: agent.EvToolCall, ...})
    }
case "user": // tool_result envelope，目前在 :528 丢弃
    if isMCPBBClawToolResult(c) {
        parsed := parseDispatchResult(c.Content)
        s.emit(agent.Event{
            Type:     agent.EvDispatchStatus,
            Dispatch: &agent.DispatchStatus{Phase: parsed.Status, TaskID: parsed.TaskID, ElapsedMs: parsed.ElapsedMs},
        })
    }
    // 其他 tool_result 仍丢弃（Phase 1 行为不变）
```

**新增类型**（`adapter/internal/agent/driver.go`）：

```go
EvDispatchStatus EventType = "dispatch_status"

// DispatchStatus 携带管家派发进度信息（ADR-021-firmware-ui §1.2）。
type DispatchStatus struct {
    Phase     string // "started" | "done" | "running" | "async" | "error"
    TaskID    string // dispatch tool_use 的 ID（或 MCP 返回的 taskId）
    Cwd       string // 派发目标项目（可选）
    ElapsedMs int64  // worker 耗时（done/async 时填写）
}
```

**httpapi NDJSON writer** 加对应帧编码：

```json
{"type":"dispatch_status","seq":N,"dispatch":{"phase":"started","taskId":"…","cwd":"proj/A"}}
{"type":"dispatch_status","seq":N,"dispatch":{"phase":"done","taskId":"…","elapsedMs":3200}}
{"type":"dispatch_status","seq":N,"dispatch":{"phase":"async","taskId":"…"}}
```

**固件接收端语义（v2 修订）**：`dispatch_status` 事件到达固件后：

1. 向顶部 `s_lbl_status`（`bb_lvgl_display.c:957-967`）临时注入文字，按 phase 优先级仲裁（见下表）
2. `done`/`error`/`async` 三态注入后 3-5 秒自动回归原文案（butler 在线时显示 `管家就绪` 等常态文案）
3. adapter 侧 `butler.Engine`（非 driver 层）订阅 `EvDispatchStatus` 事件，将每次 dispatch phase 转换 append 到 in-memory ring buffer（见 §1.4）；driver 不感知 butler 状态

**ring buffer 写入方由 `butler.Engine` 负责**，而非 `claudecode/driver.go`——driver 只发事件，Engine 订阅并落库，两者解耦。

**Phase 枚举语义**：

| phase | 含义 | 固件顶部 s_lbl_status 注入 |
|---|---|---|
| `started` | butler 调用了 dispatch 工具，worker 开始跑 | `派发中: <cwd>…` |
| `done` | worker 在 wait_seconds 内同步完成 | `worker 完成 ✅ (<elapsed>s)`，3-5s 后回归常态 |
| `running` → `async` | 超时降级，worker 转后台 | `已转异步 #<taskId>`，3-5s 后回归常态 |
| `error` | worker 失败 | `派发失败 ❌`，3-5s 后回归常态 |

**v1 局限（已接受）**：`async` 降级后 worker 最终完成，设备不会收到主动推送——用户需追问管家 `task_status`。v2 再做主动推送（worker 完成 → adapter 给管家会话注入系统消息 → 推通知到设备）。

**Cloud relay 透明**：`bbclaw-reference/cloud/internal/httpapi/agent_proxy.go` 注释已明确 "Home adapter already shapes payload into the firmware NDJSON schema; pass through unchanged"。新 event type 自动透传，**cloud 零改动**。

### 1.3 `/v1/butler/workspace/memory/stats` API（→ Mem stats，可作 E 的 stretch goal）

对齐 ADR-022 两层记忆结构，footer 渲染 `mem: <inbox>+<profile>` 时需要此端点：

```
GET /v1/butler/workspace/memory/stats?deviceId=<id>

Response 200:
{
  "inboxCount":    12,
  "profileCounts": {"preferences": 5, "projects": 3, "decisions": 4},
  "lastConsolidatedAt": "2026-06-03T10:00:00Z"  // null 若从未整理
}
```

- `inboxCount`：workspace `CLAUDE.md` 托管段的条目数（换行分割的 bullet 数）
- `profileCounts`：各 `MEMORY/*.md` 活跃区的条目数（按维度分列；不含归档尾段）
- footer 聚合显示：`mem: 12+12`（inbox + profile 总和）或 `mem: 12·5·3·4`（分列展开）
- **不阻塞 E**：footer 在拿不到 stats 时降级显示 `mem: ?`，API 留给 E 的 stretch goal 或独立小 issue

> **2026-06-10 修订**：ACTIVE 对话页底栏的 `[B] cwd | mem:N+M` 文字双格已被
> **点阵扫描条**（Knight-rider 彗星，颜色/速度随状态）取代——见
> design/UI_DESIGN_LANGUAGE.md §3 与 `bb_lvgl_display.c` 的
> `bottombar_timer_cb`。cwd / mem 统计改由**锁屏页 footer**
> (`bb_page_locked_update_footer`) 承载，派发进度仍叠加在顶栏 `s_lbl_status`；
> `memory/stats` 端点与 `s_butler_cwd` / `s_mem_*` 状态保持不变，仅 ACTIVE 底栏的
> 渲染方式变化。

### 1.4 `GET /v1/butler/dispatch/recent`（→ Sub-PR C，Task List 数据源）

替代 v1 的 session 列表，作为 Task List 页的数据源：

```
GET /v1/butler/dispatch/recent

Response 200:
[
  {"taskId":"abc","cwd":"bbclaw","title":"重构 auth","status":"running","startedAt":"…","elapsedMs":120000},
  {"taskId":"def","cwd":"docs","title":"更新 ADR-021","status":"done","elapsedMs":3200},
  {"taskId":"ghi","cwd":"another-proj","title":"lint","status":"error","error":"timeout"}
]
```

- 默认返回最近 20 条，按 `startedAt` 倒序
- `taskId` = claude `tool_use.id`（与 C 实现对齐，无需额外映射）
- `title` 取自 dispatch `tool_use` 的 input prompt，截断为 24 个 CJK 字符（或 48 字节）
- **数据源**：`butler.Engine` 维护一个 in-memory ring buffer（容量 50），订阅 `EvDispatchStatus` 事件，在 `started` 时创建条目，后续 phase 更新同一 `taskId` 的 status/elapsedMs
- **重启清空**（v1 局限，可接受）：adapter 重启后 ring buffer 为空，Task List 显示"暂无派发任务"
- LOCKED 页不显示 Task List 摘要，Task List 仅在 CHAT 页通过短按 OK 进入

---

## §2 ADR-012 "no overlay" 关系澄清

ADR-012 §1 原文针对 v0.3 的「base display + chat overlay + settings overlay 三层叠加」。

**本 ADR 引入的 footer 和 Task List 页均为 page-local layout，不是 lvgl global layer**：

- footer status bar（三页底部 ~14px）：由共享 helper `bb_status_bar_render(lv_obj_t *parent)` 创建，各页面（CHAT/SETTINGS/LOCKED）各自在页面 screen root 上调用，形成页面自身 layout 的一部分，与 `lv_layer_sys()`/`lv_layer_top()` 无关
- Task List 页：是独立的 LVGL screen，短按 OK 后全屏切换（非 overlay），BACK 返回 CHAT screen；符合 ADR-012「任何时刻只有一个页面前台」原则
- 顶部 `s_lbl_status` 注入（dispatch 状态文案）：复用 CHAT screen 现有 label，无新增子树
- **不参与 PTT 路由仲裁**：PTT 行为由 `bb_radio_app_state_t` 三态控制（ADR-012 §3 按键路由表），footer/Task List 不改变任何状态转移逻辑
- ADR-012 的「任何时刻只有一个页面前台，不再叠加 overlay」原则**不变**

因此**不需要修订 ADR-012**，在 ADR-021-firmware-ui 里澄清此关系即可。

---

## §3 短按 OK 改为 Task List 页（替代原 session picker）

按键语义保持 ADR-012 §3 表，只是 CHAT 短按 OK 的目的页从 session picker 改为 Task List（`bb_ui_agent_chat.c:2187-2280` 处的 session picker overlay 由 D 替换）：

| 按键场景 | 行为 |
|---|---|
| CHAT 短按 OK | 进入 **Task List** 页（全屏切换） |
| CHAT 长按 OK | 进入 SETTINGS（不变） |
| Task List ↑↓ | 选择行 |
| Task List OK | 自动给管家发 `task_status #<taskId>` 文本 turn → 返回 CHAT |
| Task List BACK | 返回 CHAT |

**`task_status #<taskId>` turn 的可见性**：该 turn 在 CHAT 历史中**对用户可见**——出现一行 `task_status #abc` 用户气泡属于有意设计，明确表达「用户正在追问任务进展」，比隐藏后台 turn 更透明。若后续用户体验反馈需要隐藏，可在 E/D 实现时增加「后台 turn」标记，届时修订本节。

**PTT 语义无变化**：长按仍是录音触发（ADR-012 §3 不变），无短按/长按二义性问题。v1 §3 的 Settings `show_worker_sessions` toggle 随 session 抽象一并删除。

---

## §4 资源清单

### 4.1 图标文件（待入库 `firmware/assets/icons/`）

| 文件名 | 尺寸 | 用途 | 颜色建议 |
|---|---|---|---|
| `icon_butler_16.png` | 16×16 | butler 徽标 `[B]`（Task List 行 + bottom_bar） | 主色（蓝 #2196F3 或绿 #4CAF50，沿用主题色） |
| `icon_worker_16.png` | 16×16 | worker 徽标 `[W]`（Task List 行，标注 worker 发起的任务） | 灰色 #9E9E9E |
| `icon_mem_12.png` | 12×12 | footer 记忆条数图标 | 中性灰 |
| `icon_wifi_12.png` | 12×12 | footer Wi-Fi 状态图标 | 绿（有连接）/ 红（断开） |

**顶部 dispatch 状态**：用文字 + emoji（⏳/✅/❌/⏰）注入 `s_lbl_status`，无需专门图标。

**v1 最小路径**：若图标资源未就绪，固件可先用文字前缀 `[B]`/`[W]` 替代图标，功能不阻塞；图标入库后替换渲染调用。

### 4.2 Task List 视觉规格（320×172 屏，横屏）

屏幕实际分辨率：320×172 横屏（`bb_config.h:932-937`）。

```
┌──────────────────────────────────────────────────────────────┐
│ 🏠 ⚡ Ready                              12:43  ▮▮▯ 📶 ●     │
├──────────────────────────────────────────────────────────────┤
│ 派发任务                                              3       │
│ ⏳ bbclaw 重构 services/auth          running    2分钟前      │
│▶ ✅ docs 更新 ADR-021                  done       3.2s        │
│ ❌ another-proj lint                  error      30秒前       │
├──────────────────────────────────────────────────────────────┤
│ [B] bbclaw                                       mem: 12+12  │
└──────────────────────────────────────────────────────────────┘
↑↓ 选择   OK 追问进度   ← 返回
```

- 行高 ~22px（与现有 `BB_SESSION_PICKER_ROW_H` 一致，常量保留复用）
- 每行格式：`<status emoji> <cwd> <title 截断>  <status text>  <relative time>`
- status emoji：`running=⏳  done=✅  error=❌  async=⏰`
- running 行用主色高亮；error 行红色文字；done/async 灰色
- 列表为空时显示居中文字：`暂无派发任务`
- `title` 截断：24 个 CJK 字符（或 48 字节），超出加 `…`

### 4.3 字号与颜色（沿用 `bb_lvgl_assets.c` 现有枚举）

| UI 元素 | 字号建议 | 说明 |
|---|---|---|
| 顶部 s_lbl_status 注入文字 | `BB_FONT_SMALL`（或 12px） | 复用现有 label，按 phase 临时覆写 |
| footer 状态栏文字 | `BB_FONT_TINY`（或 10px） | 底部状态栏，占 ~14px 高；须与 chat 主体文字有视觉层次区分 |
| Task List 行文字 | `BB_FONT_SMALL`（或 12px） | 行高 22px，emoji + 文字同行 |
| `[B]`/`[W]` 前缀（纯文字 fallback） | 同所在行字号 | secondary 行内嵌，颜色走主题色 / 灰色 |

### 4.4 图标产出说明

`firmware/assets/` 图标需人工产出（设计出图）。Sub-PR E 提交前须先完成图标入库；若图标未就绪，E 可先以纯文字前缀发版，图标作后续 patch 补入。

---

## §5 端到端验证用例

### 前提

`bbclaw-reference/cloud/internal/httpapi/agent_proxy.go` 已明确 NDJSON pass-through 语义：Home adapter 负责 shape payload；cloud 原样透传。新的 `dispatch_status` 帧无需 cloud 改动。

### 验证用例（C/D 落地后执行）

**用例 1：PTT 派发 → 顶部状态文案按 phase 切换**

```
前置：Adapter 启动，butler 会话已 warm，MCP server 运行
步骤：设备 PTT → "帮我在 bbclaw 项目里查一下最近的 commit"
期望：
  1. 顶部 s_lbl_status 显示 `派发中: bbclaw`（phase=started）
  2. ~3s 内更新为 `worker 完成 ✅ (3.2s)`（phase=done），3-5s 后回归常态文案
  3. 管家 TTS 播报 worker 结果摘要
```

**用例 2：短按 OK 进 Task List 看到 taskId**

```
前置：已完成用例 1，ring buffer 已有记录
步骤：在 CHAT 页短按 OK
期望：
  1. 切换到 Task List 全屏页
  2. 列表显示刚才的派发任务：`✅ bbclaw 查一下最近的 commit  done  3.2s`
  3. ↑↓ 可选行，选中行有 ▶ 标记
```

**用例 3：Task List 行 OK 触发管家追问**

```
前置：Task List 已显示，选中目标 taskId 行
步骤：按 OK
期望：
  1. 自动返回 CHAT 页
  2. CHAT 历史出现新 turn：用户气泡 `task_status #<taskId>`
  3. 管家回复该任务的最新状态
```

**用例 4：异步降级 + Task List 展示**

```
前置：wait_seconds=5 短超时
步骤：PTT → "帮我做一个大重构"
期望：
  1. s_lbl_status: `派发中: proj` → `已转异步 #<taskId>`，3-5s 后回归
  2. 短按 OK 进 Task List：该 taskId 行显示 `⏰ proj 大重构  async`
```

**用例 5：dispatch 失败（error）**

```
步骤：PTT → 请求派发到 allowlist 外路径
期望：s_lbl_status 显示 `派发失败 ❌`，3-5s 后回归；Task List 中该行显示 ❌ error
```

**用例 6：cloud_saas 模式透传验证**

```
前置：Cloud backend 运行，HomeAdapter 连云，固件烧录 cloud_saas profile
步骤：同用例 1
期望：行为与 local_home 一致（NDJSON 帧经 cloud relay 原样到达固件）
      验证：agent_proxy.go 日志中 dispatch_status 帧不被过滤/改写
```

---

## §6 Sub-PR 切分表 v2

| PR | 标题 | 依赖 | 工作量估算 |
|---|---|---|---|
| **A'（本 issue）** | `docs(adr): ADR-021-firmware-ui v2 — Task List 范式` | — | 已完成（本文件） |
| **C** | `feat(adapter): claudecode 驱动暴露 dispatch_status 事件 + /v1/butler/dispatch/recent API` | A' | ~400 LOC + 测试 |
| **D** | `feat(firmware): 砍 session picker，新建 Task List 页；短按 OK 进入；列表行 OK 触发 task_status 追问` | A', C | ~450 LOC |
| **E** | `feat(firmware): dispatch_status 文案注入顶部 s_lbl_status + bottom_bar 改 [B] cwd \| mem:N+M + LOCKED 页 footer helper` | A', C | ~350 LOC + 资源 |
| **Mem** | `feat(adapter): /v1/butler/workspace/memory/stats API` | A', ADR-022 | ~80 LOC + 测试（E 的 stretch goal） |

**C 先行**；D 依赖 C，E 依赖 C；D/E 可并行；Mem 独立，作 E 的 stretch goal 或单独 issue。Sub-PR B（menu.SessionItem Role 字段）v2 删除。

### C 的测试覆盖

- 用现有 mock claude 子进程，覆盖 `done`/`async`/`error` 三态
- 验证非 `mcp__bbclaw__` 工具的 `tool_use` 仍走 `EvToolCall`（现有行为不回归）
- 验证 `tool_result` 中非 MCP dispatch 的帧仍丢弃
- `GET /v1/butler/dispatch/recent`：ring buffer 为空返回 `[]`；ring buffer 有 5 条按 startedAt 倒序；超过 50 条时最老条目被淘汰

### D 的测试覆盖

- CHAT 短按 OK → Task List screen 渲染正确（含空列表提示）
- Task List 行 OK → 发出 `task_status #<taskId>` turn → 返回 CHAT
- Task List BACK → 返回 CHAT，无副作用

---

## §7 与 ADR-012 §3 按键路由表的完整对照

更新后的按键路由表（v2 修订行用 **粗体** 标注）：

| 页面 | UP | DOWN | LEFT | RIGHT | OK | BACK | PTT |
|---|---|---|---|---|---|---|---|
| **LOCKED** | ignore | ignore | ignore | ignore | ignore | ignore | 密语验证录音 |
| **CHAT** | 滚动历史 | 滚动历史 | 上一 driver | 下一 driver | **→ Task List**¹ | 取消 in-flight | 录音 → 发送 |
| **SETTINGS** | 上移行 | 下移行 | 值预览(-) | 值预览(+) | 提交落 NVS | → CHAT | ignore |
| **Task List** | 选择上行 | 选择下行 | — | — | **发 task_status turn → CHAT**¹ | → CHAT | ignore |

¹ v2 修订：CHAT 短按 OK 从「session picker」改为「Task List」全屏页；长按 OK 仍进 SETTINGS（ADR-012 §3 不变）。PTT 语义无任何变化。

**变化**：新增 Task List 页行（D 实现），CHAT 短按 OK 目的页从 session picker 改为 Task List。Settings 不再有 `show_worker_sessions` toggle（v2 删除）。

---

## §8 ADR 状态说明

本 ADR 状态为**草案**，等待 C/D/E 实现 PR 落地、端到端验证用例（§5）走通一次后翻为**已接受**，避免 ADR 与代码漂移。

若实现过程中发现本 ADR 有误（如 NDJSON 帧字段名与实现不符），**先改本 ADR，再改代码**（CLAUDE.md「设计先行」原则）。

---

## §9 精简主菜单 + 提醒页 + 顶栏未读徽标（v3，2026-07-03）

**关联**：[ADR-042 §10](ADR-042-adapter-v2-command-router-and-reminders.md)（提醒归 adapter，不归设备——广播投递 + 可查历史）。ADR-042 定了「提醒是 adapter 的、设备只是投递面/查询面」，本节定它在**固件 UI** 上怎么落。

### 9.0 动机与核心决策

提醒 fire 后设备要能：① 未读被看见（toast 消失后仍找得到）② 读已触发的消息 ③ 看即将到来的提醒。落地时先撞到一个**设备侧的重复**：adapter 后端是两张表——提醒 store（§ADR-042 3，你设的定时 + 调度态）与通知 outbox（§ADR-042 4，fire 后产生的 note）。一条提醒 fire 后**同时**出现在「提醒历史(done)」和「通知中心(note)」里。这个区分是**后端职责**，用户不该感知。

**决策**：
1. **设备侧把两张表呈现为一个「提醒」页**（即将 / 已提醒两段），不给用户「通知 vs 提醒」两个入口。后端两表不动（职责清晰），只在 UI 合并。
2. **待机短按 OK → 精简主菜单**（对话 / 提醒·N / 设置），设置整组降为主菜单子项。取代 v2「待机 OK 直进 SETTINGS」。
3. **顶栏常驻 🔔·N 未读徽标**，N = 「已提醒」未读数；徽标与主菜单「提醒·N」指向**同一份未读**，不各算各的。归零不显示。

> **v3.1 修订（2026-07-04，真机反馈）**：§9.1 的独立「精简主菜单」**已作废**。
> 用户要求「统一只有一个设置」——待机 OK 直进 SETTINGS，「提醒」是 Settings 列表
> 里置顶的一行（`提醒 · N`），点开即 §9.2 已提醒页。下文 §9.1 保留作历史记录，
> 实现以本框为准。未读感知落在 §9.3 的待机屏「提醒 N」徽标。

### 9.1 精简主菜单（新 LEVEL / 新 state）〔已作废，见上方 v3.1 修订〕

待机（STANDBY）短按 OK 进主菜单，而非直进 SETTINGS：

```
待机 —短按 OK→
┌─ 主菜单 ────────┐
│ 💬 对话           │  → 唤起 CHAT（等价原「任意 nav 唤醒到对话」）
│ ⏰ 提醒 · 3       │  → 提醒页（§9.2）；· N = 未读，0 时不显示计数
│ ⚙️ 设置 ▸         │  → 现有 SETTINGS 列表（Driver/Model/机器/音量/密语/更新/关于），整组不变
└──────────────────┘
转=移动光标  短按=进入  长按 BACK=退回待机
```

- 主菜单是**单层 3 行**竖列，复用 SETTINGS 的列表渲染骨架（`build_rows_box` / 光标 / footer hint），不新造控件体系。
- **设置零改动**：`bb_ui_settings` 现有 LEVEL_MAIN 及其子选择页原样作为主菜单「设置」项的下一层；仅**进入路径**从「待机 OK 直达」变成「待机 OK → 主菜单 → 设置」。
- 长按 BACK 语义不变（busy 时取消 in-flight；idle 时逐层返回，主菜单顶层再按回待机）。
- CHAT 内进菜单的手势（现为 OK/BACK → SETTINGS）本节**不改**，留作后续一致性收敛（follow-up：CHAT OK 也走主菜单）；避免本次改动面外扩。

### 9.2 提醒页（即将 / 已提醒两段——合并两张后端表）

```
┌─ 提醒 ───────────────┐
│ [即将]  已提醒         │  ← 两段，转到边界切换 / 或 LEFT/RIGHT 切段
├──────────────────────┤
│ 09:30  看烧录日志有没报错 │  即将：reminder store 中 state=scheduled，按 RunAt 升序
│ 明天   汇报 open issues  │
└──────────────────────┘
切到「已提醒」：
│ • 12:30 烧录日志已跑完… │  已提醒：outbox notes（fire 记录），按时间倒序；
│   10:05 提醒你喝水      │  行首「•」= 未读高亮，进入即视为已读（清未读→徽标递减）
```

- **即将** = 提醒 store 的 `scheduled`（未到点、可取消）。数据不在设备本地，需向 adapter 拉（§9.4）。
- **已提醒** = 通知 outbox 的 note（fire 后产生）。设备本就通过 `session.notification`（ADR-042 §3.1/§4）实时收到并可留存最近 N 条；也可随即将一起从 adapter 拉，保证多设备看同一份（ADR-042 §10 广播）。
- 每条 note 自带来源提醒信息，所以「已提醒」天然覆盖「提醒历史」，无需再单列历史入口。
- 交互：转动选行；「即将」行短按 OK → 二次确认取消该提醒（复用 prompt_select 风格）；「已提醒」只读，进入即已读。

### 9.3 顶栏 🔔·N 未读徽标

```
状态栏(ACTIVE/CHAT): [状态词 • 活动点] ……… 🔔·3  12:30  🔋 100%
```

- 位置：顶部状态栏时钟左侧（§ADR-021 现有 topbar 布局，右侧时钟/电量/WiFi 之间挤入 🔔·N）。空间紧张，N≥1 才显示，0 隐藏，保持 calm-down 后的干净顶栏。
- N = 「已提醒」未读数，与主菜单「提醒·N」同源（一个设备侧未读计数器）：收到 `session.notification` → +1 并常驻；进入「已提醒」查看 → 归零。
- 徽标只做**提示**，不可点（旋钮设备无触点）；看内容走 待机 OK → 主菜单 → 提醒。
- 待机页（STANDBY，非 CHAT）同样在其顶部或吉祥物旁给一枚小 🔔·N，保证 toast 过后待机时也看得见（待机没有 ACTIVE 顶栏）。

### 9.4 数据源与协议增量

- **未读徽标 / 已提醒**：主要来自既有 `session.notification` 推送（ADR-042 §3.1，云 WS）——设备收到即 +1、留存最近 N 条，无需新协议。
- **即将（scheduled）**：只存在于 adapter，设备原本看不到。需一条 **device→adapter 拉取**（云中继透传，kind 建议 `agent.reminders.list`），返回 `{scheduled[], history[]}`，对齐 ADR-042 §10.3 的 `GET /v1/reminders?view=`。多设备/广播下这条保证任何设备看的是**同一份 adapter 提醒**。
- 进入提醒页时拉一次；徽标计数走推送增量，二者独立即可。
- **固件侧**：`session.notification` 分支已在（ADR-042 §3.2 已加 toast + 拉取式 TTS），本节只加「未读计数 + 留存最近 N」；新增提醒页/主菜单页与 `agent.reminders.list` 请求。**云端**：透传新 kind（跨仓库，见 CLAUDE.md 协议同步表）。

### 9.5 按键路由增补（对照 §7）

| 页面 | UP/DOWN(转) | LEFT/RIGHT | OK(短按) | BACK(长按) | PTT |
|---|---|---|---|---|---|
| **STANDBY** | — | — | **→ 主菜单**（原：→ SETTINGS） | ignore | 唤起录音 |
| **主菜单** | 移动光标 | — | 进入选中项 | → STANDBY | ignore |
| **提醒页** | 选择行 | 切「即将/已提醒」段 | 即将行→取消确认；已提醒只读 | → 主菜单 | ignore |

其余页（CHAT / SETTINGS / Task List / LOCKED）本节不改。

### 9.6 落地切分与状态

- **F1（固件）**：主菜单 LEVEL/state + 待机 OK 路由改向 + 设置降为子项。纯本地，先落。
- **F2（固件）**：`session.notification` 加未读计数 + 留存；顶栏 & 待机 🔔·N 徽标渲染。
- **F3（固件+云）**：提醒页（即将/已提醒）+ `agent.reminders.list` 拉取（云端透传新 kind）；「即将」取消确认。
- 依赖：F3 的「即将」需 ADR-042 §10.3 的 adapter `?view=` API 先在（M3.5）。F1/F2 不依赖云端。

**状态**：草案（docs-only）。承接 ADR-042 §10，随其 M3.5 一并排期；实现前若与本节不符，先改本 ADR 再改代码。
