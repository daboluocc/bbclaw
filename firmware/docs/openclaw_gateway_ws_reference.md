# OpenClaw Gateway WS 协议 — BBClaw 对接参考

> 从 OpenClaw 源码提取，供 adapter 开发参考。

---

## 1. 整体架构

```
BBClaw 固件
    │
    ▼ (音频流)
BBClaw Cloud ──── ASR ──── transcript
    │
    ▼ (WS relay)
BBClaw Home Adapter ──── WS ────► OpenClaw Gateway (:18789)
                                      │
                                      ▼
                                  Agent / LLM
```

Home Adapter 通过 WebSocket 连接 Gateway，发送用户的语音转写文本，接收 AI 回复。

---

## 2. 角色（Role）

Gateway 只有 **两个** 角色：

| 角色 | 说明 | 可调用的方法 |
|------|------|-------------|
| `operator` | 人类客户端（UI、CLI、后端服务） | `agent`、`chat.send`、`chat.abort`、`sessions.*`、`send` 等 |
| `node` | 设备/外设（node-host、IoT） | `node.event`、`node.invoke.result`、`node.pending.*` 等 |

**关键规则**：
- 没有 `"control"` 角色，写 `"control"` 会报 `invalid role`
- 省略 `role` 字段时默认为 `"operator"`
- `operator` 不能调用 node 方法；`node` 不能调用 operator 方法
- BBClaw adapter 当前用 `node` 角色发 `voice.transcript`，用 `operator` 角色发 slash command（`agent` 方法）

---

## 3. Client ID 和 Mode（枚举值）

连接时 `client.id` 和 `client.mode` 必须是预定义的固定值，不能自定义。

### Client ID（`client.id` 的合法值）

| 值 | 用途 |
|----|------|
| `node-host` | Node 设备连接 |
| `gateway-client` | 后端服务/API 客户端 |
| `cli` | 命令行工具 |
| `openclaw-control-ui` | 浏览器控制面板 |
| `openclaw-tui` | 终端 UI |
| `webchat-ui` | WebChat 前端 |
| `webchat` | WebChat 后端 |
| `openclaw-macos` | macOS App |
| `openclaw-ios` | iOS App |
| `openclaw-android` | Android App |
| `test` | 测试 |
| `fingerprint` | 指纹探测 |
| `openclaw-probe` | 健康探针 |

### Client Mode（`client.mode` 的合法值）

| 值 | 用途 |
|----|------|
| `node` | 设备节点 |
| `backend` | 后端服务 |
| `cli` | 命令行 |
| `ui` | 图形界面 |
| `webchat` | WebChat |
| `probe` | 探针 |
| `test` | 测试 |

### BBClaw 使用的组合

| 场景 | client.id | client.mode | role |
|------|-----------|-------------|------|
| 发 voice.transcript | `node-host` | `node` | `node` |
| 发 slash command（agent 方法） | `gateway-client` | `backend` | `operator` |

---

## 4. 权限（Scope）

Scope 控制 `operator` 角色能调用哪些方法。`node` 角色不使用 scope。

| Scope | 包含的关键方法 |
|-------|---------------|
| `operator.admin` | 超级权限，拥有此 scope 可调用所有方法 |
| `operator.write` | `agent`、`agent.wait`、`chat.send`、`chat.abort`、`send`、`sessions.send` |
| `operator.read` | `status`、`sessions.list`、`chat.history`、`models.list` |
| `operator.approvals` | `exec.approval.*`、`plugin.approval.*` |
| `operator.pairing` | `device.pair.*`、`node.pair.*` |

**BBClaw 发 slash command 需要 `operator.write` scope**（因为 `agent` 方法在 write 组里）。

---

## 5. 认证（Auth）

### 认证方式

| 方式 | 说明 |
|------|------|
| Token | `auth.token` — Gateway 配置的共享密钥 |
| Password | `auth.password` — Gateway 配置的密码 |
| Device Token | `auth.deviceToken` — 配对后 Gateway 颁发的设备专属 token |
| Bootstrap Token | `auth.bootstrapToken` — 一次性配对 token（用完即废） |

### BBClaw 的认证策略

- **Node 连接**：用 device identity（公私钥签名）+ 可选的 `auth.token`
- **Operator 连接**：用 `auth.token`（Gateway 的共享密钥）

---

## 6. 设备配对（Device Pairing）

### 什么时候需要配对

- 设备有 identity（公钥）但不在 Gateway 的已配对列表中
- 设备已配对但请求了新的 role 或更高的 scope

### 配对流程

```
1. 设备发 connect 请求（带 device identity）
2. Gateway 发现设备未配对
3. Gateway 广播 device.pair.requested 事件
4. Gateway 返回错误 NOT_PAIRED + requestId，关闭连接
5. 用户在 Gateway UI 或 CLI 执行 device.pair.approve
6. 设备重新连接 → 配对通过 → 收到 hello-ok（含 deviceToken）
7. 后续连接可用 deviceToken 免配对
```

### 自动配对（免人工审批）

以下情况会自动批准：
- 本地客户端（同机器）的 node/operator 连接
- 使用 bootstrap token 的 node 连接
- Control UI 和 webchat 客户端

### BBClaw 的配对现状

- `node-host` 连接：首次需要配对审批，之后用 deviceToken
- `gateway-client` + `backend` 连接：本地直连 + token 认证，自动跳过配对

---

## 7. 连接握手完整流程

```
客户端                                    Gateway
  │                                         │
  │──── WS 连接 ────────────────────────────►│
  │                                         │
  │◄──── connect.challenge (含 nonce) ──────│  (仅 node 角色需要)
  │                                         │
  │──── { type:"req", method:"connect",     │
  │       params: ConnectParams } ──────────►│
  │                                         │
  │      Gateway 校验:                       │
  │      1. 协议版本 (minProtocol/maxProtocol)
  │      2. role 合法性                      │
  │      3. client.id / client.mode 枚举值   │
  │      4. 认证 (token/password/device)     │
  │      5. 设备签名验证 (node 角色)          │
  │      6. 配对检查                         │
  │                                         │
  │◄──── { type:"res", ok:true,             │
  │        payload: hello-ok } ─────────────│
  │                                         │
  │  连接建立，可以发送 RPC 请求              │
```

### ConnectParams 结构

```json
{
  "minProtocol": 3,
  "maxProtocol": 3,
  "client": {
    "id": "node-host | gateway-client | ...",
    "version": "bbclaw-adapter",
    "platform": "darwin",
    "mode": "node | backend | ...",
    "displayName": "可选的显示名",
    "deviceFamily": "可选，如 bbclaw",
    "instanceId": "可选的实例 ID"
  },
  "role": "operator | node",
  "scopes": ["operator.write"],
  "auth": {
    "token": "gateway 共享密钥",
    "deviceToken": "配对后的设备 token",
    "bootstrapToken": "一次性配对 token"
  },
  "device": {
    "id": "从公钥派生的设备 ID",
    "publicKey": "base64url 编码的公钥",
    "signature": "签名",
    "signedAt": 1712345678000,
    "nonce": "服务端提供的 nonce"
  },
  "caps": []
}
```

**关键约束**：
- 不能有 `jsonrpc` 字段（WS 协议用 `type:"req"`，不是 JSON-RPC）
- 不能有 `name` 字段（用 `displayName`）
- `additionalProperties: false` — 多余字段会被拒绝

---

## 8. 设备签名（Node 角色）

Node 连接需要设备签名。签名 payload 格式（v3）：

```
v3|{deviceId}|{clientId}|{clientMode}|{role}|{scopes逗号分隔}|{signedAtMs}|{token或空}|{nonce}|{platform}|{deviceFamily}
```

验证步骤：
1. `device.id` 必须等于从 `publicKey` 派生的 ID
2. `signedAt` 必须在服务端时间的容差范围内
3. `nonce` 必须匹配服务端发的 `connect.challenge` 里的 nonce
4. 签名用公钥验证

---

## 9. Session

Session 是 Gateway 管理对话上下文的单元。

### Session Key 格式

```
{agentId}:{requestKey}
```

例如：`agent:main:bbclaw`（agent ID 为 main，请求 key 为 bbclaw）

### BBClaw 使用的 session

- `agent:main:bbclaw` — 固件配置的默认 session key
- 所有语音交互共享同一个 session（保持对话上下文）

### Session 相关方法

| 方法 | Scope | 说明 |
|------|-------|------|
| `agent` | write | 发消息给 agent（自动创建/复用 session） |
| `chat.send` | write | 发消息到指定 session |
| `chat.abort` | write | 中止正在执行的 run |
| `chat.history` | read | 获取 session 历史 |
| `sessions.list` | read | 列出所有 session |

---

## 10. RPC 请求/响应格式

### 请求

```json
{
  "type": "req",
  "id": "唯一请求 ID",
  "method": "方法名",
  "params": { ... }
}
```

### 成功响应

```json
{
  "type": "res",
  "id": "对应请求 ID",
  "ok": true,
  "payload": { ... }
}
```

### 错误响应

```json
{
  "type": "res",
  "id": "对应请求 ID",
  "ok": false,
  "error": {
    "code": "ERROR_CODE",
    "message": "描述"
  }
}
```

### 事件（服务端推送）

```json
{
  "type": "event",
  "event": "事件名",
  "payload": { ... }
}
```

---

## 11. BBClaw Adapter 的两种连接

### 连接 A：Node 角色（voice.transcript）

```json
{
  "type": "req",
  "id": "connect-xxx",
  "method": "connect",
  "params": {
    "minProtocol": 3,
    "maxProtocol": 3,
    "client": {
      "id": "node-host",
      "displayName": "bbclaw-adapter",
      "version": "bbclaw-adapter",
      "platform": "darwin",
      "deviceFamily": "bbclaw",
      "mode": "node",
      "instanceId": "bbclaw-adapter"
    },
    "role": "node",
    "caps": [],
    "device": { "id": "...", "publicKey": "...", "signature": "...", "signedAt": ..., "nonce": "..." },
    "auth": { "token": "..." }
  }
}
```

用途：发送 `node.event` + `voice.transcript`，接收 `chat` 事件获取 AI 回复。

### 连接 B：Operator 角色（slash command / agent）

```json
{
  "type": "req",
  "id": "connect-xxx",
  "method": "connect",
  "params": {
    "minProtocol": 3,
    "maxProtocol": 3,
    "client": {
      "id": "gateway-client",
      "version": "bbclaw-adapter",
      "platform": "darwin",
      "mode": "backend"
    },
    "role": "operator",
    "scopes": ["operator.write"],
    "auth": { "token": "..." }
  }
}
```

用途：发送 `agent` 方法调用 slash command（`/stop`、`/new`、`/status`），Gateway 会解析命令并执行。

---

## 12. 常见错误

| 错误 | 原因 |
|------|------|
| `invalid role` | role 不是 `operator` 或 `node` |
| `invalid connect params: at /client/id: must be equal to constant` | client.id 不在枚举列表中 |
| `invalid connect params: at /client/mode: must be equal to constant` | client.mode 不在枚举列表中 |
| `unexpected property 'jsonrpc'` | WS 帧不能有 jsonrpc 字段 |
| `unexpected property 'name'` | client 里不能有 name 字段（用 displayName） |
| `unauthorized role: node` | node 角色调用了 operator 方法（如 agent） |
| `NOT_PAIRED` + `PAIRING_REQUIRED` | 设备未配对，需要审批 |
