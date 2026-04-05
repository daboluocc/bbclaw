# AI 工具调用过程实时展示

## 背景

当 AI 需要调用工具（如 `web_fetch` 查网站、`curl` 执行命令等）时，处理时间可能远超普通对话（30 秒以上）。之前固件在等待回复时没有任何过程反馈，容易因超时误判为失败。

## 解决方案

### 超时策略：idle timeout 替代固定 deadline

Adapter 等待 Gateway 回复时，从固定 deadline（默认 25 秒）改为 **idle timeout**：

- 收到任何 Gateway WebSocket 消息就刷新倒计时
- 只有连续 25 秒无消息才判定超时
- 配置项：`OPENCLAW_REPLY_WAIT_SECONDS`（adapter `.env`）

这样 AI 调工具期间 Gateway 持续发送 `agent` 事件，不会误超时。

### 过程事件透传

通过三层 relay 将 AI 的 thinking 和 tool_call 事件传递到设备屏幕：

```
Gateway (session.message 事件)
  → Adapter (operator 双连接，解析 thinking/toolCall)
    → Cloud (relay 到设备 WS)
      → 固件 (显示在聊天区域)
```

#### Adapter 双连接架构

Adapter 对每次 voice transcript 请求建立两个 Gateway WebSocket 连接：

| 连接 | 角色 | 用途 |
|------|------|------|
| Node 连接 | `node` | 发送 `node.event` (voice.transcript)，接收 `chat` 事件 |
| Operator 连接 | `operator` | 订阅 `sessions.messages.subscribe`，接收 `session.message` 事件（含 thinking/toolCall） |

Gateway 协议限制：`session.message` 事件需要 `operator.read` scope，`node.event` 方法只允许 `node` 角色调用，因此必须双连接。

#### 事件类型

| 事件 | 来源 | 固件显示 |
|------|------|----------|
| `thinking` | AI 内部推理 | `[thinking...]` |
| `tool_call` | AI 调用工具 | `[tool: web_fetch]` |
| `reply.delta` | AI 回复文本 | 实际回复内容 |

#### 固件显示效果

聊天区域逐行追加过程信息：

```
[thinking...]
[tool: web_fetch]
✅ 正常，没问题
```

过程记录保留在聊天历史中，不走 TTS 播报。

### 超时配置一览

| 层 | 配置 | 默认值 |
|---|---|---|
| 固件 HTTP | `BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS` | 90 秒 |
| Adapter 等 Gateway | `OPENCLAW_REPLY_WAIT_SECONDS` | 25 秒（idle） |
| Adapter 内部上限 | 硬编码 | 120 秒 |

### 支持模式

- **local_home**：Adapter HTTP streaming 直接传 thinking/tool_call 事件
- **cloud_saas**：Adapter → Cloud WS relay → 固件 WS
