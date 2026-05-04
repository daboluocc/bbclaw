# ADR-013: 设备端会话历史回放与上下翻页

- **日期**: 2026-05-04
- **状态**: 已接受
- **关联**: ADR-001（Adapter as Agent Bus）、ADR-002（Multi-turn session lifecycle）、ADR-003（Router + multi-driver）、`design/multi_session_management.md`

## 背景

S1/S2 阶段（commit `9ea4c4d`）落地了多 session 管理：用户从 picker 选一个旧 session，
固件用其 `sessionId` 续接 adapter，从此后续 turn 走 resume 路径。但**首次进入旧 session 时
transcript 是空的**——只显示当前 turn 之后的消息，所有历史"看不见"。

根因（探索结论）：

- **协议层零历史**：`POST /v1/agent/message` 流式 NDJSON 只回当前 turn；`GET /v1/agent/sessions`
  只返回 `SessionInfo` 元数据（`preview` / `messageCount`），没下发完整消息体。
- **固件 UI 无缓存**：transcript 是 LVGL flex column；每条消息一个 `lv_label`，进会话时
  `on_exit/on_enter` 直接重建空容器，没有"先回放历史再续接"的钩子。
- **Adapter 有数据没接口**：`claudecode` driver 把完整对话以 JSONL 持久化在
  `~/.claude/projects/{cwdHash}/{sid}.jsonl`，但只在 `ListSessions` 里读首行做 preview。

体验后果：用户从 picker 重新进入 30 分钟前的会话，看到的是空白屏 + 一个 sessionId 角标。
要查历史只能去 web。这违反了 "device-first chat" 的初心。

## 决策

加一对协议端点和一段 UI 流水线，让设备能：(a) 进会话时立即回放最近一批历史；(b) 用户上翻
到顶时懒加载更早的批次。

### 1. 协议（adapter ↔ device）

新增 endpoint:

```
GET /v1/agent/sessions/{id}/messages?driver=<name>&before=<seq>&limit=<n>
→ {
    "ok": true,
    "data": {
      "messages": [
        {"role": "user",      "content": "...", "seq": 0},
        {"role": "assistant", "content": "...", "seq": 1},
        ...
      ],
      "total": 348,
      "hasMore": true
    }
  }
```

- `messages` 永远 chronological 升序（旧→新），客户端无需排序。
- `seq` 是 adapter 在解析 JSONL 时的 0-based filtered 索引——过滤掉 tool_call / system 后的
  "干净消息"序号。作为分页游标使用。
- `before <= 0`（典型首次调用） = 拉最末 `limit` 条；`before = K` = 拉
  `[max(0, K-limit), K)` 这一段，配合 `min(seq)` 实现"向上翻页"。
- `limit` 默认 50，硬上限 200（保护设备内存）。
- 不支持 `MessageLoader` 的 driver 返回 `{"ok":false,"error":"MESSAGES_NOT_SUPPORTED"}` 200
  响应——固件做静默降级（与今天行为一致：空 transcript），不阻断会话功能。

### 2. Adapter 实现

引入可选接口（沿用 ADR-003 的 SessionLister 模式）：

```go
type Message struct {
    Role    string `json:"role"`
    Content string `json:"content"`
    Seq     int    `json:"seq"`
}

type MessagesPage struct {
    Messages []Message `json:"messages"`
    Total    int       `json:"total"`
    HasMore  bool      `json:"hasMore"`
}

type MessageLoader interface {
    LoadMessages(ctx context.Context, sid string, before, limit int) (MessagesPage, error)
}
```

- `claudecode` driver 实现：扫描 JSONL → 过滤 user/assistant → 文本块拼接 → 切片 + 分页。
- 单条消息 content 在 adapter 端硬截断 4 KB（小屏不需要更长）。
- 其他 driver（aider/ollama/openclaw）暂不实现；HTTP handler 用 type assertion 做能力探测。
- cloud_saas driver 后续单独 PR（cloud 仓闭源，需要先在那里加 sink）。

### 3. 固件实现

**agent client** (`bb_agent_client.[ch]`)：新增 `bb_agent_load_messages(...)`，模仿
`bb_agent_list_sessions` 的同步阻塞 + dyn buffer + cJSON 解析模式。返回堆分配的
`bb_agent_message_t` 数组（caller-owned，配套 `bb_agent_messages_free`）。

**theme**：`bb_agent_theme_t` 加三个可选方法：
- `append_history_message(role, content)` — 末尾追加，**不**设 `active_assistant`，避免历史
  assistant 消息被后续流式 chunk 误拼。
- `prepend_history_message(role, content)` — 顶部插入（`lv_obj_move_to_index(lbl, 0)`）。
- `is_transcript_at_top()` — 上翻懒加载触发条件。

text-only 实现这三个；buddy-ascii / buddy-anim 留 NULL，UI 层 NULL-check。

**chat UI** (`bb_ui_agent_chat.c`)：
- 加 history 状态字段：`history_total / history_min_seq / history_loaded_count /
  history_has_more / history_fetch_pending / history_fetch_generation`。
- `spawn_history_fetch_task(before, is_initial)`：worker 任务调
  `bb_agent_load_messages`，结果通过 `lv_async_call(on_history_fetch_done, ...)` 回 LVGL 线程。
- 触发点：
  1. `bb_ui_agent_chat_show()` 时如果 NVS 里有 saved session_id —— 拉首页。
  2. `bb_ui_agent_chat_session_picker_select()` 切到一个旧 session —— 重置 + 拉首页。
  3. `bb_ui_agent_chat_scroll(lines<0)` 滚到 transcript 顶时（`is_transcript_at_top`），如有
     `has_more` 且 `loaded_count < BB_HISTORY_MAX_LOADED` —— 拉下一批
     （`before = history_min_seq`）。
- in-DOM 上限 `BB_HISTORY_MAX_LOADED = 300` —— 超过就停止懒加载（避免 LVGL 节点数失控）。
- 单页 `BB_HISTORY_PAGE_SIZE = 50`。

### 4. 内存预算

| 项 | 估算 |
|---|---|
| 单条 message struct | 16B role + 8B ptr + 4B seq ≈ 32B + content 堆字符串 |
| 单条 content（典型） | 200B–800B（adapter 已截到 4 KB） |
| 一页 50 条 | ~25 KB transit 内存 |
| 在 DOM 中 300 条 | LVGL label 对象 ~150B/条 + 平均 500B 文本 = ~200 KB |

ESP32-S3 + PSRAM 下完全可承受；纯内部 RAM 也够（LVGL 对象走 PSRAM 池）。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 让固件每次进 session 发一条空 marker 触发 adapter 内部 resume | adapter 进程持有了历史，但 UI 还看不到——没有任何用户体验改善。 |
| 一次性下发全部历史，不分页 | 千条消息会卡死 LVGL 渲染 + 撑爆 RAM；分页是必须的。 |
| 把分页游标做成时间戳（`?before_ts=...`） | claudecode JSONL 没有 per-row 时间戳；要加得改 CLI。用 seq（行号）零依赖。 |
| 双向无限滚动 | 没有"未来消息"——session 末尾就是最新，新消息走流式。下翻没意义。 |
| 在固件 NVS 缓存历史 | 浪费 flash 寿命，且新会话/多设备同步没好处；adapter/cloud 已经是真相源。 |

## 后续工作

- v2: 在 transcript 顶部显示一个加载占位 label "Loading earlier messages..."，加载完
  `lv_obj_del`。v1 仅日志。
- v2: 维持滚动位置补偿——prepend 后用新内容总高度补偿 `scroll_y`，让用户视觉位置不跳。v1
  接受跳动（用户已经在顶部，跳到内容仍可读）。
- v3: cloud_saas driver 实现 `MessageLoader`（要在 cloud 仓里加）。
- v3: tool_call 历史回放——目前只回放 user/assistant 文本，工具调用不显示。
