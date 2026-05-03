# Multi-Session Management & Notification System

- **日期**: 2026-05-03
- **状态**: Draft
- **关联**:
  - [ADR-003](decisions/ADR-003-router-and-multi-driver.md) — 多 driver 路由 + session 绑定
  - [ADR-012](decisions/ADR-012-fixed-page-menu.md) — 三态页面状态机
  - [AGENT_STATE_MACHINE](AGENT_STATE_MACHINE.md) — 9 态 buddy 模型

## 概述

在单个 BBClaw 设备上实现多 session 并发管理：

1. **Session 选择器** — CHAT 页面按 OK 触发 session 弹窗，滚动选择当前 driver 下的 session
2. **Driver / Session 双层切换** — LEFT/RIGHT 切 driver，OK 进入 session picker
3. **后台 session 通知** — adapter 接管的并发 session 完成任务时，通过 WebSocket 实时推送通知到设备，显示未读计数（类似短信机制）

## 统一 WebSocket 通知架构

**核心决策**：两种 transport 模式（`local_home` / `cloud_saas`）统一使用 WebSocket 推送通知，消除双模式分支。

| 维度 | local_home | cloud_saas |
|------|-----------|-----------|
| **请求路径** | Firmware → Adapter (LAN) | Firmware → Cloud → Adapter |
| **通知通道** | **WebSocket 推送**（Adapter WS Server） | **WebSocket 推送**（复用已有 Cloud WS） |
| **Session 存储** | Adapter 内存 | Adapter 内存（Cloud 透传） |
| **通知延迟** | <50ms | <1s |

**为什么统一 WS**：
- Firmware 通知逻辑只有一套代码路径，无 `bb_transport_is_cloud_saas()` 分支
- local_home 通知延迟从 5-15s（轮询）降到 <50ms（推送）
- 未来 voice streaming、config sync 等也可迁移到 WS，最终两种模式传输层完全统一
- Adapter 新增 WS server 成本低（gorilla/websocket 已在依赖中）

### 架构图

```
┌──────────────────────────────────────────────────────┐
│  Firmware (ESP32)                                     │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────┐   │
│  │ Session     │  │ Notification │  │ Chat Page  │   │
│  │ Picker UI   │  │ Badge/Toast  │  │ (existing) │   │
│  └──────┬──────┘  └──────┬───────┘  └─────┬─────┘   │
│         │                │                 │         │
│         └────────────────┼─────────────────┘         │
│                          │                           │
│              ┌───────────┴───────────┐               │
│              │   bb_notification.h   │               │
│              │   (WebSocket 接收)     │               │
│              └───────────┬───────────┘               │
│                          │ WebSocket                 │
│              ┌───────────┴───────────┐               │
│              │  esp_websocket_client │               │
│              └───────────┬───────────┘               │
└──────────────────────────┼───────────────────────────┘
                           │
          ┌────────────────┼────────────────┐
          │ local_home     │                │ cloud_saas
          │ (LAN WS)       │                │ (WAN WS)
          ▼                │                ▼
┌─────────────────┐        │     ┌─────────────────────┐
│ Adapter          │        │     │ Cloud Server         │
│ ┌─────────────┐ │        │     │ ┌─────────────────┐ │
│ │ WS Server   │ │        │     │ │ WebSocket Hub   │ │
│ │ /ws (NEW)   │ │        │     │ │ (existing)      │ │
│ ├─────────────┤ │        │     │ │ + notification  │ │
│ │ HTTP Server │ │        │     │ │   relay         │ │
│ │ (existing)  │ │        │     │ └─────────────────┘ │
│ ├─────────────┤ │        │     │         ↕ WS        │
│ │ Session     │ │        │     │ ┌─────────────────┐ │
│ │ Registry    │ │        │     │ │ Agent Proxy     │ │
│ │ + Notify    │ │        │     │ │ (HTTP→WS)       │ │
│ └─────────────┘ │        │     │ └─────────────────┘ │
└─────────────────┘        │     └─────────────────────┘
                           │                ↕ WS
                           │     ┌─────────────────────┐
                           │     │ Adapter (same)       │
                           │     └─────────────────────┘
                           │
```

---

## 用户场景

### 场景 1: 本地多 session 开发

```
用户在 BBClaw 上同时开了 3 个 claude-code session：
  - session-1: 正在重构 auth 模块（后台运行中）
  - session-2: 修 bug #42（已完成，等待查看）
  - session-3: 当前活跃对话

设备显示：
  ┌──────────────────────┐
  │ claude-code  [2 new]  │  ← topbar: driver + 未读计数
  │                       │
  │  (^_^) ready          │  ← buddy state for active session
  │                       │
  │  > 重构完成，已提交 PR │  ← session-3 的最新消息
  └───────────────────────┘

用户按 OK → 弹出 session picker：
  ┌──────────────────────┐
  │ Sessions (3)         │
  │                      │
  │ ▸ [●] auth重构  NEW  │  ← session-1 有新消息
  │   [●] fix#42   NEW  │  ← session-2 有新消息
  │   [○] 当前对话  ◀    │  ← session-3 (active)
  │   [+] 新建 session   │
  │   ⚙ Settings         │
  └──────────────────────┘

UP/DOWN 滚动，OK 切换到选中 session，BACK 返回
```

### 场景 2: 后台 session 完成通知（实时推送）

```
session-1 的 auth 重构完成：
  1. Adapter turn_end → WS 推送 notification 给设备
  2. 设备立即收到（<50ms），显示 toast + badge

  ┌──────────────────────────┐
  │ claude-code  [1 new]      │
  │                           │
  │  (^_^) ready              │
  │                           │
  │ ┌────────────────────────┐│
  │ │ ✓ auth重构: PR已创建    ││  ← toast (2s)
  │ └────────────────────────┘│
  └───────────────────────────┘
```

### 场景 3: cloud_saas 密语解锁后进入

```
cloud_saas 模式开机：
  LOCKED → PTT 密语验证 → CHAT (自动恢复上次 session)
                                ↓
                          WebSocket 已连接（经 Cloud relay）
                          立即收到积压的通知
```

---

## Phase 划分

| Phase | 功能 | 涉及层 |
|-------|------|--------|
| S1 | Session Picker UI（列表 + 切换） | Firmware |
| S2 | Adapter WS Server + session/notification API | Adapter |
| S3 | WebSocket 通知推送（adapter → device） | Adapter + Cloud + Firmware |
| S4 | Firmware 通知 UI（未读 badge + toast） | Firmware |
| S5 | Session 详情预览 | 全栈 |

---

## Phase S1: Session Picker UI

### 按键路由变更

| 键 | 当前行为 | 新行为 |
|----|---------|--------|
| LEFT/RIGHT | 切换 driver | **不变** |
| OK | → SETTINGS | **变更** — 打开 Session Picker |
| OK (picker内) | — | 选中 session / 新建 / 进 Settings |
| BACK (picker内) | — | 关闭 picker |
| BACK (CHAT) | 取消 in-flight | 不变 |

**Settings 入口**：下沉到 Picker 底部行。

> 理由：OK 是"我要做点什么"的自然操作，session 切换比进 Settings 更高频。

### Picker 状态机

```c
typedef enum {
  PICKER_HIDDEN = 0,
  PICKER_LOADING,       // 从 adapter 拉取 session 列表
  PICKER_VISIBLE,       // 列表显示，等待选择
  PICKER_SWITCHING,     // 切换中
} bb_session_picker_state_t;
```

### Picker 数据模型

```c
#define BB_SESSION_PICKER_MAX_ITEMS  8

typedef struct {
  bb_agent_session_info_t sessions[BB_SESSION_PICKER_MAX_ITEMS];
  int count;
  int selected;           // 当前高亮行
  int active_idx;         // 活跃 session index (-1 if not found)
  int unread[BB_SESSION_PICKER_MAX_ITEMS];
  bb_session_picker_state_t state;
} bb_session_picker_t;
```

### Picker UI 布局（128x64 OLED）

```
┌──────────────────────────┐
│ Sessions (claude-code)   │  ← 标题：driver 名
├──────────────────────────┤
│ ▸ auth重构     ●2        │  ← 选中行，未读 2
│   fix-bug#42   ●1        │  ← 未读 1
│   当前对话     ◀         │  ← 活跃标记
│   + 新建                 │
│   ⚙ Settings            │
└──────────────────────────┘
```

- 可见 5 行，超出滚动
- preview 截断 14 字符 + 未读 badge

### Picker 按键

| 键 | 行为 |
|----|------|
| UP/DOWN | 移动选中行（wrap） |
| OK | 确认选择 |
| BACK | 关闭 picker |
| LEFT/RIGHT | 关闭 picker + 切 driver |

### Session 切换流程

```
OK (session 行)
  → save session_id → bb_session_store_save()
  → clear transcript
  → buddy state = IDLE
  → close picker
  → 下次 PTT 使用新 session_id
```

### 新建 Session

```
OK ([+ 新建])
  → clear session_id (空字符串)
  → clear transcript
  → buddy state = IDLE
  → 下次 PTT 时 adapter 自动创建新 session
  → 收到 SESSION 事件后保存
```

---

## Phase S2: Adapter WebSocket Server + API 增强

### 新增：Adapter WebSocket Server

Adapter 新增 WebSocket 端点 `/ws`，供 local_home 模式下 firmware 建立长连接：

```go
// internal/httpapi/server.go — 新增路由
r.HandleFunc("/ws", h.handleWebSocket)
```

#### 连接管理

```go
type DeviceConn struct {
    conn     *websocket.Conn
    deviceID string       // local_home 下可为空或固定值
    mu       sync.Mutex
    closed   bool
}

type WSHub struct {
    mu      sync.Mutex
    devices map[string]*DeviceConn  // deviceID → conn
}
```

local_home 模式下通常只有一个设备连接，简化为单连接管理。

#### 消息格式（复用 cloud 的 Envelope 格式）

```json
{
  "type": "event",
  "kind": "session.notification",
  "payload": {
    "sessionId": "cc-xxx",
    "driver": "claude-code",
    "type": "turn_end",
    "preview": "PR #42 已创建",
    "timestamp": 1714700123000
  }
}
```

Firmware → Adapter（ACK）：
```json
{
  "type": "request",
  "kind": "session.notification.ack",
  "payload": { "sessionId": "cc-xxx" }
}
```

#### 为什么复用 Envelope 格式

- cloud_saas 模式下 firmware 已经在解析这个格式
- 统一格式意味着 firmware 的 WS message handler 不需要区分来源
- Adapter 的 WS server 和 Cloud hub 使用相同的消息协议

### 现有 API（已实现）

```
GET /v1/agent/sessions?driver=<name>&limit=6
→ {"ok":true, "data":{"sessions":[{id, preview, lastUsed, messageCount}]}}
```

### 新增 HTTP API

#### 删除 Session

```
DELETE /v1/agent/sessions/<sessionId>
→ {"ok":true}
```

#### Session 状态查询（批量）

```
GET /v1/agent/sessions/status?driver=<name>&ids=sid1,sid2,sid3
→ {"ok":true, "data":{"statuses":{
    "sid1": {"state":"running", "lastEvent":"tool_call", "updatedAt":1714700000},
    "sid2": {"state":"completed", "lastEvent":"turn_end", "updatedAt":1714699000},
    "sid3": {"state":"idle", "lastEvent":null, "updatedAt":1714698000}
  }}}
```

Session 状态：`idle` | `running` | `completed` | `error`

---

## Phase S3: WebSocket 通知推送

### 通知数据格式（统一）

```json
{
  "sessionId": "cc-xxx",
  "driver": "claude-code",
  "type": "turn_end",
  "preview": "PR #42 已创建，等待 review",
  "timestamp": 1714700123000
}
```

### 事件产生时机

Adapter 在以下时刻推送通知：

1. **turn_end** — 非当前活跃 session 的 turn 完成
2. **error** — 后台 session 出错
3. **tool_approval** — 后台 session 需要 tool 审批（Phase 2）

"当前活跃 session"：最近 60s 内收到过该 session 的 `/v1/agent/message` 请求。

### 通知分发路径

```
Adapter: session turn_end (非活跃 session)
  │
  ├─ local_home:
  │   → WSHub.Send(deviceConn, envelope)
  │   → 设备 WS client 立即收到
  │
  └─ cloud_saas:
      → Adapter WS client → Cloud Hub → Device WS
      → 设备立即收到
```

**两种模式下 adapter 的通知产生逻辑完全相同**，只是发送目标不同：
- local_home: 发到本地 WSHub 的 device conn
- cloud_saas: 发到 cloud 的 WS client（已有连接）

```go
// internal/httpapi/notifications.go
func (h *Handler) pushNotification(notif SessionNotification) {
    env := Envelope{
        Type: "event",
        Kind: "session.notification",
        Payload: notif,
    }

    if h.wsHub != nil {
        // local_home: 推送到本地连接的设备
        h.wsHub.Broadcast(env)
    }

    if h.cloudConn != nil {
        // cloud_saas: 推送到 cloud（cloud 转发给设备）
        h.cloudConn.Send(env)
    }
}
```

### Cloud Hub 处理（cloud_saas 模式）

Cloud hub 收到 `session.notification` 后：

1. **设备在线** → 直接转发到设备 WS
2. **设备离线** → 积压到 per-device 队列（上限 32 条，FIFO）
3. **设备重连** → flush 积压

```go
case "session.notification":
    device := h.getDevice(env.DeviceID)
    if device != nil {
        WriteEnvelope(device, env)
    } else {
        h.pendingNotifications.Enqueue(env.DeviceID, env)
    }
```

### Adapter 积压队列（local_home 模式）

local_home 下设备 WS 断连时，adapter 本地积压：

```go
type NotificationQueue struct {
    mu     sync.Mutex
    events []SessionNotification
    maxLen int // 32
}
```

设备重连后 flush。逻辑与 cloud hub 对称。

### ACK 机制

设备通过 WS 发送 ACK：

```json
{"type":"request", "kind":"session.notification.ack", "payload":{"sessionId":"cc-xxx"}}
```

- local_home: adapter 直接处理
- cloud_saas: cloud hub 转发给 adapter

---

## Phase S4: Firmware 通知接收 + 未读 Badge + Toast

### WebSocket 连接（统一）

Firmware 在两种模式下都建立 WebSocket 连接：

```c
// bb_notification.c
esp_err_t bb_notification_start(void) {
    // 两种模式都走 WebSocket，只是 URL 不同
    const char* ws_url;
    if (bb_transport_is_cloud_saas()) {
        ws_url = build_cloud_ws_url();  // 已有实现，复用
    } else {
        ws_url = build_adapter_ws_url(); // 新增：ws://adapter-ip:port/ws
    }
    return bb_ws_connect(ws_url, on_ws_message);
}
```

**cloud_saas 模式**：复用已有的 WebSocket 连接（`bb_adapter_client.c` 中已建立），只需在 message handler 中新增 `session.notification` 分支。

**local_home 模式**：新建一条 WebSocket 连接到 adapter 的 `/ws` 端点。

### WebSocket Message Handler（统一）

```c
// 两种模式共用同一个 handler
static void on_ws_notification(const char* kind, cJSON* payload) {
    if (strcmp(kind, "session.notification") == 0) {
        bb_notification_t notif;
        parse_notification_payload(payload, &notif);
        bb_notification_store_add(&notif);
        lv_async_call(on_notification_received, NULL);
    }
}
```

### 通知存储（RAM）

```c
#define BB_NOTIFY_MAX  16

typedef struct {
    char session_id[64];
    char driver[24];
    char preview[48];
    int64_t timestamp;
    uint8_t type;           // 0=turn_end, 1=error, 2=tool_approval
    uint8_t read;           // 0=unread, 1=read
} bb_notification_t;

typedef struct {
    bb_notification_t items[BB_NOTIFY_MAX];
    int count;
    int unread_total;
    SemaphoreHandle_t lock; // WS 回调任务 + LVGL 任务并发保护
} bb_notification_store_t;
```

### 未读 Badge

Topbar（theme 渲染）：

```
claude-code [3]     ← unread > 0 时显示
```

Theme 接口新增：
```c
void (*set_unread_count)(int count);
```

### Toast 通知

```
┌──────────────────────────┐
│ claude-code  [1 new]     │
│                          │
│  (^_^) ready             │
│                          │
│ ┌──────────────────────┐ │
│ │ ✓ auth重构: PR已创建 │ │  ← 2s 自动消失
│ └──────────────────────┘ │
└──────────────────────────┘
```

- 不打断当前操作
- 多条排队（每条 2s）
- 任意键关闭

### 未读清除时机

- 切换到某 session → 该 session 未读清零 + 发 ACK
- 在某 session 内发消息 → 该 session 未读清零 + 发 ACK

### 断连处理

WS 断连时通知暂停，重连后对端（adapter 或 cloud）flush 积压。Firmware 无需 fallback 轮询逻辑。

---

## Phase S5: Session 详情预览

Picker 中 RIGHT 键展开详情：

```
┌──────────────────────────┐
│ auth重构                 │
├──────────────────────────┤
│ Driver: claude-code      │
│ Messages: 24             │
│ Last: 2min ago           │
│ Status: completed        │
│                          │
│ "PR #42 已创建，等待..."  │
│                          │
│ [OK: 切换] [BACK: 返回]  │
└──────────────────────────┘
```

---

## 按键路由总表

| 页面/模式 | UP | DOWN | LEFT | RIGHT | OK | BACK | PTT |
|---|---|---|---|---|---|---|---|
| **CHAT** (normal) | 滚动↑ | 滚动↓ | prev driver | next driver | **打开 Picker** | 取消 turn | 录音发送 |
| **CHAT** (picker) | 选中↑ | 选中↓ | 关闭+prev driver | 关闭+next driver | **确认选择** | 关闭 picker | ignore |
| **SETTINGS** | 上移行 | 下移行 | 值(-) | 值(+) | 提交 NVS | → CHAT | ignore |
| **LOCKED** | — | — | — | — | — | — | 密语验证 |

---

## 数据流时序图

### Session 列表拉取

```
User          Firmware              Adapter (direct or via Cloud)
 │               │                     │
 │──OK──────────→│                     │
 │               │──GET /sessions──────→│
 │               │←─session list───────│
 │  (显示 picker) │                     │
```

### 后台通知推送（两种模式统一）

#### local_home

```
Adapter (WS Server)         Firmware (WS Client)        UI
 │                              │                       │
 │  (session-1 turn_end)        │                       │
 │──WS: session.notification───→│                       │
 │                              │──lv_async_call───────→│
 │                              │                       │  badge [1]
 │                              │                       │  toast
 │                              │                       │
 │              (user switches to session-1)             │
 │                              │                       │
 │←─WS: notification.ack───────│                       │
 │  mark acknowledged           │                       │  badge [0]
```

#### cloud_saas

```
Adapter         Cloud Hub         Firmware (WS)        UI
 │                 │                   │                │
 │ (turn_end)      │                   │                │
 │──WS: notif────→│                   │                │
 │                 │──WS push────────→│                │
 │                 │                   │──lv_async────→│
 │                 │                   │                │ badge [1]
 │                 │                   │                │ toast
 │                 │                   │                │
 │                 │←─WS: ack─────────│                │
 │←─WS: ack──────│                   │                │ badge [0]
```

### 设备离线 → 重连 → flush

```
Adapter/Cloud              Firmware
 │                            │ (offline)
 │  (session-1 turn_end)      │
 │  → enqueue pending         │
 │  (session-2 error)         │
 │  → enqueue pending         │
 │                            │
 │       (device reconnects)  │
 │                            │
 │──flush 2 notifications────→│
 │                            │──lv_async → badge [2], toast x2
```

---

## 对现有代码的影响

### Firmware 变更

| 文件 | 变更 |
|------|------|
| `bb_ui_agent_chat.c` | OK 键改为打开 picker；新增 picker 状态机 |
| `bb_ui_agent_chat.h` | 新增 picker API |
| `bb_radio_app.c` | OK 键路由变更 |
| `bb_adapter_client.c` | cloud_saas WS handler 新增 `session.notification` 分支 |
| 新文件: `bb_notification.h/c` | 通知接收 + 存储 + badge/toast + local_home WS 连接 |
| Theme 接口 | 新增 `set_unread_count()`、`show_toast()` |

### Adapter 变更

| 文件 | 变更 |
|------|------|
| 新文件: `internal/httpapi/ws.go` | WebSocket server + DeviceConn 管理 |
| 新文件: `internal/httpapi/notifications.go` | NotificationQueue + 推送逻辑 |
| `internal/httpapi/server.go` | 注册 `/ws` 路由 + notification HTTP 端点 |
| `internal/agent/router.go` | turn_end 时触发通知推送 |

### Cloud 变更

| 文件 | 变更 |
|------|------|
| `internal/router/hub.go` | `session.notification` 事件路由 + 积压队列 + 重连 flush |
| `internal/router/hub.go` | `session.notification.ack` 转发 |

---

## 风险与约束

1. **ESP32 RAM** — 通知存储 16 条，picker 8 条。FIFO 淘汰。local_home WS 连接额外占用 ~4KB。
2. **Adapter WS server** — 新增组件，但 gorilla/websocket 已在依赖中，实现量小。
3. **Adapter 重启** — 通知队列丢失。可接受（用户查看 session 列表即可恢复上下文）。
4. **Session 过期** — sweeper 30min 淘汰 idle session。Picker 需处理 "session not found"。
5. **并发安全** — WS 回调写入 notification store + LVGL 任务读取，需 semaphore。
6. **通知风暴** — 6 个 session 同时跑时可能短时间大量通知。合并同 session 连续通知 + toast 排队缓解。
7. **local_home WS 断连** — adapter 本地积压（上限 32），重连后 flush。无需 HTTP fallback。

---

## 开放问题

1. **Picker 触发键** — 是否需要长按 OK 直接进 Settings 的快捷方式？
2. **通知声音** — 硬件有 buzzer 吗？是否需要蜂鸣提示？
3. **Session 命名** — 用户能否给 session 起名？还是只用 preview 自动生成？
4. **跨 driver 通知** — topbar 显示总未读还是当前 driver 未读？建议总未读。
5. **最大并发 session** — adapter 侧限制？建议上限 6。
6. **local_home WS 复用** — 未来 voice streaming 是否也迁移到这条 WS？建议 v2 考虑。

---

## 实现优先级

```
Week 1: Phase S1 (Picker UI, mock 数据) + Phase S2 (Adapter WS server + API)
Week 2: Phase S3 (通知推送全链路) + Phase S4 (badge + toast UI)
Week 3: Phase S5 (详情预览) + Cloud hub 集成 + 端到端联调
```

**关键路径**：S2 (Adapter WS server) 是基础设施，S1/S4 是用户体验，并行开发。
