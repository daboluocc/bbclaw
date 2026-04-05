# OpenClaw Gateway WS Protocol — BBClaw 对接参考

> 从 OpenClaw 源码 (`/Volumes/1TB/github/openclaw`) 提取，供 adapter 开发参考。

## 1. 角色（Role）

Gateway 只有两个角色（`src/gateway/role-policy.ts`）：

| 角色 | 可调用方法 | 说明 |
|------|-----------|------|
| `operator` | `agent`, `chat.send`, `chat.abort`, `send`, `sessions.*` 等 | 主角色，可操作 agent |
| `node` | `node.event`, `node.invoke.result`, `node.pending.*` 等 | 外设角色，只能发 node 事件 |

**注意**：没有 `"control"` 角色。connect params 里 `role` 字段只接受 `"operator"` 或 `"node"`。省略 `role` 时默认为 `"operator"`。

## 2. Client ID（必须是枚举常量）

`client.id` 必须是以下值之一（`src/gateway/protocol/client-info.ts`）：

```
webchat-ui | openclaw-control-ui | openclaw-tui | webchat | cli
gateway-client | openclaw-macos | openclaw-ios | openclaw-android
node-host | test | fingerprint | openclaw-probe
```

## 3. Client Mode（必须是枚举常量）

`client.mode` 必须是以下值之一：

```
webchat | cli | ui | backend | node | probe | test
```

## 4. Connect 请求格式

```json
{
  "type": "req",
  "id": "<unique-id>",
  "method": "connect",
  "params": {
    "minProtocol": 3,
    "maxProtocol": 3,
    "client": {
      "id": "<GatewayClientId 枚举值>",
      "version": "<非空字符串>",
      "platform": "<非空字符串>",
      "mode": "<GatewayClientMode 枚举值>",
      "displayName": "<可选>",
      "deviceFamily": "<可选>",
      "instanceId": "<可选>"
    },
    "role": "operator | node",
    "auth": {
      "token": "<gateway token>"
    },
    "device": { ... },
    "scopes": ["admin", "read", "write", "approvals", "pairing"],
    "caps": []
  }
}
```

**关键约束**（`additionalProperties: false`）：
- 不能有 `jsonrpc` 字段
- 不能有 `name` 字段（用 `displayName`）
- `client.id` 和 `client.mode` 必须是枚举值，不能自定义

## 5. 方法权限（Scope）

`agent` 方法需要 `write` scope（`src/gateway/method-scopes.ts`）：

| Scope | 包含方法 |
|-------|---------|
| `write` | `agent`, `agent.wait`, `chat.send`, `chat.abort`, `send`, `sessions.send`, `sessions.abort` ... |
| `read` | `status`, `sessions.list`, `chat.history`, `models.list` ... |
| `admin` | `config.set`, `sessions.delete`, `chat.inject` ... |

node 角色的方法是独立集合：`node.event`, `node.invoke.result`, `node.pending.drain` 等。

## 6. BBClaw Adapter 连接模板

### Node 连接（voice.transcript 用）

```json
{
  "client": {
    "id": "node-host",
    "version": "bbclaw-adapter",
    "platform": "darwin",
    "mode": "node",
    "deviceFamily": "bbclaw"
  },
  "role": "node",
  "device": { "id": "...", "publicKey": "...", "signature": "...", "signedAt": ..., "nonce": "..." }
}
```

### Operator 连接（agent / slash command 用）

```json
{
  "client": {
    "id": "gateway-client",
    "version": "bbclaw-adapter",
    "platform": "darwin",
    "mode": "backend"
  },
  "role": "operator",
  "auth": { "token": "<gateway-token>" },
  "scopes": ["write"]
}
```

## 7. Agent 方法调用

```json
{
  "type": "req",
  "id": "<unique-id>",
  "method": "agent",
  "params": {
    "message": "/status",
    "sessionKey": "agent:main:bbclaw"
  }
}
```

响应通过 `event: "agent"` 事件流返回：
- `stream: "assistant"` — 文本 chunk
- `stream: "tool"` — 工具调用
- `stream: "lifecycle"` + `phase: "end"` — 执行完成

## 8. 取消执行

```json
{
  "type": "req",
  "id": "<unique-id>",
  "method": "chat.abort",
  "params": { "runId": "<run-id>" }
}
```
