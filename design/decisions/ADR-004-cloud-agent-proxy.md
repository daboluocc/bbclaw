# ADR-004: cloud_saas 模式下的 Agent Bus 代理

- **编号**: ADR-004
- **标题**: cloud 端代理 `/v1/agent/*` 到 home adapter
- **日期**: 2026-04-25
- **状态**: 已接受
- **相关文档**: [agent_bus.md](../agent_bus.md)、[firmware_agent_integration.md](../firmware_agent_integration.md)、[ADR-001](ADR-001-adapter-as-agent-bus.md)

## 背景

Phase 4.0–4.2 把固件 Agent Chat 通路打通：设备 ↔ adapter HTTP `/v1/agent/message`（NDJSON 流）。

实际真机调试发现一个**架构断层**：

- 设备主流量（PTT 语音 / TTS）走 **cloud_saas** —— 设备 ↔ `bbclaw.daboluo.cc` ↔ home adapter（家里 Mac 跑 adapter）
- 但 Agent Bus 当前固件是**直连本地 adapter**（`BBCLAW_ADAPTER_BASE_URL`，比如 `http://192.168.10.26:18080`）
- 这要求设备和 adapter **必须同一局域网**，否则失败（实测 `ESP_ERR_HTTP_CONNECT`）

用户期望：cloud_saas 模式下，**所有通路都走云**，包括 Agent Bus —— 不应该让设备直连家里 Mac 的 LAN 地址。

## 决策

> **2026-04-27 注**：本 ADR 写于 adapter 还在 `bbclaw-reference/adapter` 时。
> Adapter 已搬到本仓 `adapter/`（commit `bf24299`），下文路径里的
> `bbclaw-reference/adapter/...` 均对应当前的 `adapter/...`。Cloud 仍在闭源仓。

**云端（`bbclaw-reference/cloud`）新增 Agent Bus 反向代理**：

- 暴露 `POST /v1/agent/message` 和 `GET /v1/agent/drivers` 端点
- 通过设备已建立的 cloud_relay WebSocket（device ↔ cloud ↔ home_adapter）转发 HTTP 请求/流式响应到目标 home adapter
- 设备认证已存在（`home_site_id` + token）—— cloud 用它定位目标 adapter

固件侧：`bb_agent_client.c` 的 `agent_base_url()` **已经按 transport profile 切换**（commit 后 cloud_saas → `BBCLAW_CLOUD_BASE_URL`）。设备这边一行不用再动。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 设备硬编 home adapter 局域网 IP | 不稳定（DHCP / 防火墙）、出门就废、需要每台设备配一次 |
| cloud_saas 模式下禁用 Agent Chat | 设备最大用户群是 cloud_saas，禁用等于砍核心功能 |
| 设备先 ICMP 探测局域网 adapter，找不到再降级 cloud | 探测成本 + 失败模式复杂；用户可能在公司 WiFi 但 Mac 在家 |
| cloud 自己实现 Agent Bus 全套（router、driver、subprocess 管理） | 要在云上跑 claude-code 子进程，鉴权和资源隔离都重；违反 ADR-001 "记忆归 CLI" |

## 实现计划（Phase 4.8）

### cloud 端（`bbclaw-reference/cloud/internal/...`）

1. 新 module `cloud_agent_proxy.go`（或类似命名）：
   - 接收设备 `POST /v1/agent/message` HTTP 请求
   - 通过该设备的 cloud_relay WS 通道发一个新的 wire frame（比如 `{"type":"agent.proxy.request", "method":"POST", "path":"/v1/agent/message", "body":..., "stream":true}`）
   - home adapter 收到后调本地 adapter 的 `/v1/agent/message`，把 NDJSON 流通过 WS 反推回云
   - 云端把 WS 流式 frame 重新组装成设备的 HTTP NDJSON 响应

2. home adapter 端（`bbclaw-reference/adapter/internal/homeadapter`）增 `agent.proxy.*` 帧处理

3. `GET /v1/agent/drivers` 同样代理

### 鉴权 / 路由

- 设备 → cloud：现有 device identity / 注册 token 一套
- cloud → home adapter：现有 cloud_relay WS，已经认证过

### 边界情况

- home adapter 离线：cloud 立即返回 503 + 友好错误
- WS 中断中流：cloud 给设备发一个 `error` 帧并关闭流
- 多 home adapter 共用一个云：按 `home_site_id` 路由（设备携带）

## 后果

### 正向

- 设备在任何 WiFi（咖啡店、移动热点）都能用 Agent Chat —— 不要求和 home adapter 同 LAN
- 复用现有 cloud_relay 通道，不引入新的连接
- 鉴权链不变，无新攻击面
- 固件零改（已经按 profile 选 URL）

### 负向

- 多一跳延迟（设备 → cloud → home adapter，~50–150ms 额外往返）
- cloud 端新增 stateful 转发逻辑，需要 stream 测试
- WS 这一段过于"通用 HTTP 隧道" —— 未来好用但要小心别被乱用变成代理万物

### 开放问题

- 一个 home_site_id 可能对应多个 adapter（家里 + 办公室）—— Phase 4.8 默认选第一个；后续按 driver capability 路由
- Adapter 离线时设备 UX：是不是离线缓存最近一条对话（设备端 NVS）？— 留 ADR-005

## 立即行动

- 固件改动已 ready（commit pending），cloud_saas 设备会去连云
- **下一步开发：cloud_agent_proxy 实现** —— `bbclaw-reference/cloud` 加路由 + ws 帧处理；`bbclaw-reference/adapter` 加 home_adapter 端的 proxy 处理
- 实现期间设备**装 cloud_saas 跑不了 Agent Chat** —— 临时验证可以切到 `local_home` profile（直连本地 adapter）
