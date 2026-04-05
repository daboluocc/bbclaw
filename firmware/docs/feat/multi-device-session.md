# Feature: 多设备并行 Session（Multi-Device Concurrent Sessions）

> 状态：已实现（P0）
> 创建：2026-04-05

## 1. 目标

多台 BBClaw 设备同时接入同一个 bbclaw-adapter（或 Cloud），各自持有独立 session，并行运行互不干扰。

典型场景：

- 家庭内多个房间各放一台 BBClaw，共用一个 Home Adapter
- 教室/办公室多台设备同时在线，各自独立对话

## 2. 现状分析

### 2.1 固件侧

| 项 | 现状 | 问题 |
|----|------|------|
| `BBCLAW_DEVICE_ID` | `BBClaw-<ver>-<MAC后3字节>`，运行时唯一 | ✅ 天然唯一，无需改 |
| `BBCLAW_SESSION_KEY` | 编译期硬编码 `"agent:main:bbclaw"` | ❌ 所有设备共享同一 session，对话上下文互相污染 |
| `stream/start` 请求 | 带 `deviceId` + `sessionKey` | sessionKey 相同时 Gateway 视为同一会话 |

### 2.2 Adapter 侧（`references/adapter`）

| 项 | 现状 | 问题 |
|----|------|------|
| `audio.Manager` | `maxConcurrent` 可配，已支持多流并发 | ✅ 结构上已就绪 |
| OpenClaw WS 连接 | node 连接 1 条，operator 连接按需拨号 | 需确认 node.event 是否支持不同 sessionKey 并行投递 |
| `voice.transcript` 投递 | 带 `sessionKey` 字段 | ✅ 只要 sessionKey 不同，Gateway 会路由到不同 session |

### 2.3 OpenClaw Gateway

| 项 | 现状 |
|----|------|
| Session Key 格式 | `{agentId}:{requestKey}`，如 `agent:main:bbclaw` |
| 多 session 并行 | Gateway 本身支持多 session 同时存在 |
| node.event 路由 | 按 `sessionKey` 分发到对应 session |

### 2.4 Cloud 侧

| 项 | 现状 |
|----|------|
| `stream start/chunk/finish` | 按 `deviceId` + `streamId` 区分流 |
| Home Adapter relay | 按 `deviceId` + `sessionKey` 转发 transcript |

## 3. 核心结论

> **关键瓶颈只有一个：固件端 `sessionKey` 是编译期常量，所有设备共享同一值。**

Gateway、Adapter、Cloud 在协议层面已经支持多 session 并行——只要每台设备发送不同的 `sessionKey`，各自的对话上下文就是隔离的。

## 4. 方案

### 4.1 Session Key 生成策略

将 `sessionKey` 从编译期常量改为运行时按设备生成：

```
agent:main:{deviceSuffix}
```

其中 `deviceSuffix` 取自 `BBCLAW_DEVICE_ID` 的 MAC 后缀部分（如 `A1B2C3`），保证：

- 同一设备重启后 sessionKey 不变（对话上下文可延续）
- 不同设备 sessionKey 不同（隔离）

实现方式：

```c
// bb_config.h 或 bb_identity.c
const char *bbclaw_session_key(void);
// 返回 "agent:main:A1B2C3"（MAC 后缀）
```

### 4.2 固件改动范围

| 文件 | 改动 |
|------|------|
| `bb_identity.c` | 新增 `bbclaw_session_key()` 函数 |
| `bb_config.h` | `BBCLAW_SESSION_KEY` 改为调用 `bbclaw_session_key()` |
| `bb_adapter_client.c` | 已通过宏引用，无需改 |
| `bb_radio_app.c` | 已通过宏引用，无需改 |
| `bb_gateway_node.c` | 已通过宏引用，无需改 |

### 4.3 Adapter 侧改动

理论上无需改动——Adapter 已经透传 `sessionKey`，Gateway 按 key 路由。

需验证：

- [ ] `audio.Manager.maxConcurrent` 默认值是否 >= 预期设备数
- [ ] Home Adapter WS relay 是否正确按 `sessionKey` 分发回复到对应设备
- [ ] Cloud `stream finish` 的 NDJSON 回复是否按 `deviceId` 隔离

### 4.4 可选增强：自定义 Session Key 前缀

如果用户希望多台设备共享同一对话上下文（如"家庭共享助手"），可通过 menuconfig 配置 session key 前缀：

```
BBClaw -> Session -> Session Key Prefix  (默认 "agent:main")
BBClaw -> Session -> Session Isolation    (per-device / shared)
```

`per-device` 模式：`{prefix}:{deviceSuffix}`
`shared` 模式：`{prefix}:bbclaw`（当前行为，所有设备共享）

## 5. 验证计划

### 5.1 本地验证（无需真机）

1. 启动 OpenClaw Gateway + bbclaw-adapter
2. 用 curl 模拟两台设备，分别发送不同 `deviceId` + `sessionKey`：

```bash
# 设备 A
curl -X POST http://127.0.0.1:18080/v1/stream/start \
  -H "Content-Type: application/json" \
  -d '{"deviceId":"BBClaw-dev-AAA","sessionKey":"agent:main:AAA","streamId":"ptt-a-1","codec":"opus","sampleRate":16000,"channels":1}'

# 设备 B
curl -X POST http://127.0.0.1:18080/v1/stream/start \
  -H "Content-Type: application/json" \
  -d '{"deviceId":"BBClaw-dev-BBB","sessionKey":"agent:main:BBB","streamId":"ptt-b-1","codec":"opus","sampleRate":16000,"channels":1}'
```

3. 验证两个 stream 可以并行存在，finish 后各自拿到独立的 transcript/reply
4. 在 Gateway 侧确认创建了两个独立 session

### 5.2 真机验证

1. 两台 ESP32-S3 烧录同一固件（MAC 不同 → sessionKey 自动不同）
2. 同时接入同一 Adapter
3. 同时按 PTT 录音，确认各自独立完成 ASR → reply 流程
4. 检查 display 上各自显示自己的对话内容

### 5.3 边界验证

- Adapter `maxConcurrent` 达到上限时，后来的设备应收到明确错误（`ErrBusy`）
- 设备断线重连后，sessionKey 不变，对话上下文可恢复
- Cloud 模式下同样验证隔离性

## 6. 风险与注意事项

| 风险 | 缓解 |
|------|------|
| Gateway 对同一 node 连接的并发 session 数有限制 | 查阅 Gateway 配置；必要时 Adapter 为每个 session 建独立 node 连接 |
| Adapter 内存/CPU 随设备数线性增长 | 设合理上限（如 `maxConcurrent=8`）；监控 Adapter 资源 |
| 回复路由：finish 的 NDJSON 回复如何路由回正确设备 | HTTP 请求-响应模型天然隔离（每个设备自己的 HTTP 连接拿自己的回复） |
| Cloud TTS/display 回复路由 | Cloud 按 `deviceId` 路由，已有机制 |

## 8. Cloud Hub 修复：旧连接驱逐

调研发现 Cloud `router/hub.go` 的 `Register()` 在同一 `deviceID` 或 `homeSiteID` 重新连接时，直接覆盖 map entry 但不关闭旧 WS 连接。旧 goroutine 仍在阻塞读，导致：

- 旧设备的 `handleDeviceWS` 不退出，占用资源
- Home Adapter 重连后旧连接残留，回复可能路由到已失效的 Peer

修复：`Register()` 覆盖前主动 `Close()` 旧连接，使旧 goroutine 立即收到 EOF 退出。

改动文件：`references/cloud/internal/router/hub.go`

1. **P0**：固件 `sessionKey` 改为 per-device（核心改动，1 个函数）
2. **P1**：Adapter `maxConcurrent` 默认值调整 + 配置暴露
3. **P2**：menuconfig 增加 session isolation 选项
4. **P3**：display 上显示当前 session 标识（多设备调试用）
