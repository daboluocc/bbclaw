# ADR-021-firmware-ui: 管家模式固件 UI —— 角色徽标 / 派发状态条 / 底部状态栏 / PTT 文案

- **日期**: 2026-06-03
- **状态**: 草案（docs-only；B/C/D/E 四个实现 PR 由本 ADR 解锁，独立排期）
- **关联**:
  - [ADR-021](ADR-021-conversational-orchestrator-butler.md)（对话式编排管家——事件源基础）
  - [ADR-022](ADR-022-memory-consolidation-and-profile-docs.md)（记忆整理——inbox/profile 两层；§1 memory stats API 对齐本 ADR）
  - [ADR-019](ADR-019-server-driven-menu-protocol.md)（server-driven 菜单协议——menu.SessionItem 扩展契约）
  - [ADR-012](ADR-012-fixed-page-menu.md)（固定三页菜单——footer 不违反 no-overlay 原则，见 §2）
  - Issue [#97](https://github.com/daboluocc/bbclaw/issues/97)（本 issue）

---

## 背景

ADR-021 v1（#78 合并，子 issue #84–#88 / #83 全部 merged）已在 **Adapter 层**落地对话式编排管家：

- `logicalsession.Role`（RoleButler / RoleWorker）已定义，`ListDeviceFacing` 已过滤 worker
- `butlermcp/server.go` + `cmd/mcp-server` 子命令已托管 `dispatch/list_projects/task_status/task_result` 工具
- `butler.Engine` 通过 `Deps.MemoryWriter` 在每个 butler turn 末写入 workspace `CLAUDE.md` 收件箱
- ADR-022（#91 merged）已落地 consolidation 引擎，`workspace/MEMORY/preferences.md` / `projects.md` / `decisions.md` 三层画像文档就绪

但**固件 UI 没有任何视觉/交互变化**——管家在工作，设备对用户是"隐形"的。

本 ADR 的任务是**先把协议契约和架构决策落到纸面**，作为后续四个实现 PR（B/C/D/E）的单一设计源。

---

## §1 协议增量（B/C/Mem 的契约源头）

### 1.1 menu.SessionItem 加 `Role` 字段（→ Sub-PR B）

**adapter 侧改动**：

```go
// adapter/internal/agent/menu/menu.go
type SessionItem struct {
    ID         string
    Title      string
    Driver     string
    CwdName    string
    LastUsedAt time.Time
    Role       string // "butler" | "worker" | "" (RoleNone, 向后兼容)
}
```

- `GET /v1/agent/menu/sessions` 默认行为不变：调用 `manager.ListDeviceFacing`，worker 会话仍被过滤
- 新增 query 参数 `?include=workers`：切换为调用 `manager.List`（完整列表），worker 行以灰色 + `[W]` 前缀呈现
- `role` 字段在现有 `secondary` 文本里已足够（设备渲染 `secondary` 即可），无需改 `Row` 的顶层 schema
- butler 会话的 `secondary` 应在 adapter 侧加 `[B]` 标注前缀，例如 `[B] claude-code · 3 分钟前 · 12 条`

**向后兼容**：老固件忽略 `secondary` 里的 `[B]` 前缀文字，渲染不变。新固件通过 `SessionItem.Role` 字段做颜色/图标区分。

### 1.2 dispatch_status 事件——复用 tool_use / tool_result（→ Sub-PR C）

**关键架构决策**：不新建跨进程 IPC 通道。MCP server（`cmd/mcp-server`）是 claude CLI 起的 stdio 子进程，跨进程注入要搞 socket/file IPC，不必要。

**更好的路径**：claude CLI 本身就把 `mcp__bbclaw__dispatch` 工具调用以 stream-json 的 `tool_use` / `tool_result` 帧吐出来。`claudecode/driver.go` 当前行为：

- `:510` `case "tool_use"` → 发 `EvToolCall`（所有工具统一处理）
- `:528` `tool_result`（`user` envelope）→ **当前丢弃**

**新行为**（仅针对 `mcp__bbclaw__*` 前缀工具，其他保持不变）：

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

**Phase 枚举语义**：

| phase | 含义 | 固件 UI |
|---|---|---|
| `started` | butler 调用了 dispatch 工具，worker 开始跑 | `派发中: <cwd>…` |
| `done` | worker 在 wait_seconds 内同步完成 | `worker 完成 ✅ (3.2s)` |
| `running` → `async` | 超时降级，worker 转后台 | `已转异步 #<taskId>` |
| `error` | worker 失败 | `派发失败 ❌` |

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

---

## §2 ADR-012 "no overlay" 关系澄清

ADR-012 §1 原文针对 v0.3 的「base display + chat overlay + settings overlay 三层叠加」。

**本 ADR 引入的 dispatch_bar 和 footer 均为 page 自身 LVGL 子树，不是 lvgl global layer**：

- `dispatch_bar`（chat page 顶部 ~16px）：是 `bb_ui_agent_chat.c` chat root 下的一个子 `lv_obj_t`，与 `lv_layer_sys()`/`lv_layer_top()` 无关
- footer status bar（三页底部 ~14px）：由共享 helper `bb_status_bar_render(lv_obj_t *parent)` 创建，各页面（CHAT/SETTINGS/LOCKED）各自在页面 screen root 上调用，形成页面自身 layout 的一部分
- **不参与 PTT 路由仲裁**：PTT 行为由 `bb_radio_app_state_t` 三态控制（ADR-012 §3 按键路由表），footer/dispatch_bar 不改变任何状态转移逻辑
- ADR-012 的「任何时刻只有一个页面前台，不再叠加 overlay」原则**不变**

因此**不需要修订 ADR-012**，在 ADR-021-firmware-ui 里澄清此关系即可。

---

## §3 PTT 长按二义性决策

### 问题

原 issue #97 提案：「长按 PTT 切换"显示全部 / 仅 butler"」。

### 拒绝原方案的理由

PTT 在三态中已承载两种录音语义：

| 状态 | PTT 语义 |
|---|---|
| LOCKED | 密语验证录音 |
| CHAT | 普通语音 turn 录音 |
| SETTINGS | 忽略 |

再加第三态（长按切换 worker 显示）违背 ADR-012 §3 修复「PTT 行为不可预测」的初衷，且 CHAT 态下长按本身已是录音触发方式（区分短按/长按的时间阈值在硬件层不稳定）。

### 替代方案（已接受）

**Settings 页面新增一行 toggle**：

```
显示 worker 会话: [ 否 ] / [ 是 ]
```

- LEFT/RIGHT 预览，OK 提交落 NVS
- key：`show_worker_sessions`（bool，默认 `false`）
- 效果：`否`（默认）→ 菜单调用 `GET /v1/agent/menu/sessions`（ListDeviceFacing，过滤 worker）；`是` → 加 `?include=workers`（完整列表，含 worker 灰色行）
- 与 ADR-012 §3 Settings 页面 LEFT/RIGHT/OK 语义完全一致，无新增按键语义

---

## §4 资源清单

### 4.1 图标文件（待入库 `firmware/assets/icons/`）

| 文件名 | 尺寸 | 用途 | 颜色建议 |
|---|---|---|---|
| `icon_butler_16.png` | 16×16 | butler 会话徽标 `[B]` | 主色（蓝 #2196F3 或绿 #4CAF50，沿用主题色） |
| `icon_worker_16.png` | 16×16 | worker 会话徽标 `[W]` | 灰色 #9E9E9E |
| `icon_mem_12.png` | 12×12 | footer 记忆条数图标 | 中性灰 |
| `icon_wifi_12.png` | 12×12 | footer Wi-Fi 状态图标 | 绿（有连接）/ 红（断开） |

**v1 最小路径**：若图标资源未就绪，固件可先用文字前缀 `[B]`/`[W]` 替代图标，功能不阻塞；图标入库后替换渲染调用。

### 4.2 字号与颜色（沿用 `bb_lvgl_assets.c` 现有枚举）

| UI 元素 | 字号建议 | 说明 |
|---|---|---|
| dispatch_bar 文字 | `BB_FONT_SMALL`（或 12px） | 顶部状态行，占 ~16px 高 |
| footer 状态栏文字 | `BB_FONT_TINY`（或 10px） | 底部状态栏，占 ~14px 高；须与 chat 主体文字有视觉层次区分 |
| `[B]`/`[W]` 前缀（纯文字 fallback） | 同所在行字号 | secondary 行内嵌，颜色走主题色 / 灰色 |

### 4.3 图标产出说明

`firmware/assets/` 图标需人工产出（设计出图）。Sub-PR E 提交前须先完成图标入库；若图标未就绪，E 可先以纯文字前缀发版，图标作后续 patch 补入。

---

## §5 cloud_saas 模式 dispatch 事件端到端验证用例

### 前提

`bbclaw-reference/cloud/internal/httpapi/agent_proxy.go` 已明确 NDJSON pass-through 语义：Home adapter 负责 shape payload；cloud 原样透传。新的 `dispatch_status` 帧无需 cloud 改动。

### 验证用例（C 落地后执行）

**用例 1：local_home 模式同步完成（done）**

```
前置：Adapter 启动，butler 会话已 warm，MCP server 运行
步骤：设备 PTT → "帮我在 bbclaw 项目里查一下最近的 commit"
期望：
  1. 固件 dispatch_bar 显示 `派发中: bbclaw`（phase=started）
  2. ~3s 内 dispatch_bar 更新为 `worker 完成 ✅ (3.2s)`（phase=done）
  3. 管家 TTS 播报 worker 结果摘要
```

**用例 2：local_home 模式异步降级（async）**

```
前置：同上，但 wait_seconds=5（短超时，强制触发降级）
步骤：设备 PTT → "帮我在 big-project 里做一个完整重构"
期望：
  1. dispatch_bar 显示 `派发中: big-project`（phase=started）
  2. ~5s 后 dispatch_bar 显示 `已转异步 #<taskId>`（phase=async）
  3. 管家 TTS 告知"任务已转后台，你可以稍后问我进展"
```

**用例 3：cloud_saas 模式 done（验证 cloud 透传）**

```
前置：Cloud backend 运行，HomeAdapter 连云，固件烧录 cloud_saas profile
步骤：同用例 1
期望：行为与 local_home 一致（NDJSON 帧经 cloud relay 原样到达固件）
      验证：`agent_proxy.go` 日志中 dispatch_status 帧不被过滤/改写
```

**用例 4：dispatch 失败（error）**

```
步骤：PTT → 请求派发到 allowlist 外路径
期望：dispatch_bar 显示 `派发失败 ❌`，管家 TTS 解释原因
```

---

## §6 Sub-PR 切分表

| PR | 标题 | 依赖 | 工作量估算 |
|---|---|---|---|
| **A（本 issue）** | `docs(adr): ADR-021-firmware-ui 设计草稿` | — | 已完成（本文件） |
| **B** | `feat(adapter): menu.SessionItem 加 Role + ?include=workers` | A | ~150 LOC + 测试 |
| **C** | `feat(adapter): claudecode 驱动暴露 dispatch_status 事件` | A | ~250 LOC + 测试 |
| **D** | `feat(firmware): session picker 区分 butler/worker + Settings 显示开关` | A, B | ~200 LOC |
| **E** | `feat(firmware): CHAT 派发状态条 + 三页底部状态栏 + PTT 文案` | A, C | ~600 LOC + 资源 |
| **Mem** | `feat(adapter): /v1/butler/workspace/memory/stats API` | A, ADR-022 | ~80 LOC + 测试（E 的 stretch goal） |

**B/C 可并行**（相互不依赖）；D 依赖 B，E 依赖 C；Mem 独立，作 E 的 stretch goal 或单独 issue。

### B 的单测覆盖

- 默认请求：`GET /v1/agent/menu/sessions`（无 include 参数）→ worker 会话被过滤，role=butler 行显示 `[B]` 前缀
- `?include=workers` → worker 行出现，role=worker 行有 `[W]` 前缀，跨 device 不泄漏

### C 的端到端测试

- 用现有 mock claude 子进程，覆盖 `done`/`async`/`error` 三态
- 验证非 `mcp__bbclaw__` 工具的 `tool_use` 仍走 `EvToolCall`（现有行为不回归）
- 验证 `tool_result` 中非 MCP dispatch 的帧仍丢弃

---

## §7 与 ADR-012 §3 按键路由表的完整对照

更新后的按键路由表（新增行为用 **粗体** 标注）：

| 页面 | UP | DOWN | LEFT | RIGHT | OK | BACK | PTT |
|---|---|---|---|---|---|---|---|
| **LOCKED** | ignore | ignore | ignore | ignore | ignore | ignore | 密语验证录音 |
| **CHAT** | 滚动历史 | 滚动历史 | 上一 driver | 下一 driver | → SETTINGS | 取消 in-flight | 录音 → 发送 |
| **SETTINGS** | 上移行 | 下移行 | 值预览(-) | 值预览(+) | 提交落 NVS | → CHAT | ignore |

**变化**：Settings 新增 `show_worker_sessions` toggle 行（D 实现），其按键语义与现有 Settings 行完全一致（LEFT/RIGHT 预览，OK 落 NVS）。PTT 语义**无变化**。

---

## §8 ADR 状态说明

本 ADR 状态为**草案**，等待 B/C 实现 PR 落地、端到端验证用例（§5）走通一次后翻为**已接受**，避免 ADR 与代码漂移。

若实现过程中发现本 ADR 有误（如 NDJSON 帧字段名与实现不符），**先改本 ADR，再改代码**（CLAUDE.md「设计先行」原则）。
