# BBClaw 项目画像

> 整体架构全景 + 设计文档脉络梳理
> 生成日期：2026-05-23 ｜ 当前版本：`v0.4.3`（HEAD = `v0.4.3-17-g575d85b`，含 Unreleased 改动）
> 本文档为**导览性质**的项目画像，不是真相来源；真相以 `design/` 下的设计文档与 ADR 为准。

---

## 1. 一句话定位

BBClaw 是 **Claude Code / OpenCode / Aider / Ollama / OpenClaw 等 Agent CLI 的语音外设**——一台类对讲机的硬件设备，通过桌面常驻的 **Adapter** 与电脑上的 AI agent 通信，让用户**不在电脑边上也能接管和操作任务**。

体验灵感来自传统对讲机（按住说话、实时语音）与 BB 机/寻呼机（推送、轻量摘要），由**同一套固件**统一承载，而非两种可切换的「模式」。产品形态接近 Siri / Alexa：用户从不关心后台是哪一个 conversation，只关心「我和我的 BBClaw 一直在聊」。CLI 的实现细节（`cwd`、CLI session id、`--resume` 行为）对设备**完全透明**，由 Adapter 或 Cloud 控制台代管（见 ADR-014）。

---

## 2. 四端架构全景

```
┌──────────────┐   HTTP+device bearer   ┌──────────────┐   WS envelope   ┌──────────────┐   spawn/stdio   ┌──────────────┐
│  ESP32 设备   │ ─────────────────────► │    Cloud     │ ──────────────► │   Adapter    │ ──────────────► │  Agent Driver │
│  (firmware)  │ ◄───────────────────── │   (:38081)   │ ◄────────────── │   (:18080)   │ ◄────────────── │ Claude Code/  │
└──────────────┘     局域网模式可直连     └──────────────┘                 └──────────────┘                 │ Ollama/Aider…│
       ▲              Adapter :18080            ▲                                                            └──────────────┘
       │                                        │ HTTP+session token
       │                                  ┌──────────────┐
       └── 局域网直连(Phase1 LAN-only) ──  │  Web / 未来   │
                                          │  Mobile App  │
                                          └──────────────┘
```

数据流（公网 SaaS 路径）：`ESP32 设备 ↔ Cloud (:38081) ↔ Home Adapter (:18080) ↔ Agent Driver`。

两条请求通道在 Cloud 侧最终汇入同一批 hub kind（`agent.sessions`、`agent.message` 等），只是鉴权与设备解析不同：

| 通道 | 入口 | 鉴权 | deviceId 来源 |
|------|------|------|---------------|
| 设备面 `/v1/agent/*` | Cloud | device bearer token | query/body |
| 门户面 `/v1/portal/agent/*` | Cloud | 用户 session（`bbclaw.portal.token`） | `resolvePortalDeviceID` → 首个 active 绑定 |

### 2.1 仓库切分（开源 / 闭源）

| 仓库 | 可见性 | 内容 |
|------|--------|------|
| `daboluocc/bbclaw`（主仓） | 开源 | `firmware/`（ESP32 固件，C / ESP-IDF）、`adapter/`（Go Agent Bus 守护进程）、`docs/`、`design/`、ADR |
| `bbclaw-reference`（私仓） | 闭源（护城河） | `cloud/`（Go 云控制面：账户/计费/多租户路由/ASR·TTS/OTA）、`web/`（React 用户门户）、`promo/`（落地页） |

Adapter 于 **2026-04-27（ADR-011）** 从 `bbclaw-reference` 迁入主仓开源，module path 不变（`github.com/daboluocc/bbclaw/adapter`）。理由：cloud 和 web 是 ToC 真正护城河，而 adapter 只是把各 CLI 封装成统一 HTTP/WS 接口的桥，无专有算法、无云配置、无数据残留。

---

## 3. 四端职责与代码现状

### 3.1 Firmware — ESP32-S3 固件（`firmware/`，约 44 个 `.c`）

设备侧硬件与 UI。芯片 ESP32-S3（8MB Flash），音频 ES8311 CODEC，显示 1.47″ ST7789（横屏 320×172，LVGL），交互为 PTT 按键 + 振动马达 + Flipper 风格 6 键导航。

关键模块：

| 文件 | 职责 |
|------|------|
| `bb_radio_app.c` | 主应用 / 状态机调度、按键路由 |
| `bb_ptt.c` | PTT 按键状态机（IDLE / CAPTURING / WAITING） |
| `bb_adapter_client.c` | cloud_saas WS 客户端，含 `session.notification` 分支 |
| `bb_agent_client.c` | Agent session 客户端（拉列表、续接、切换） |
| `bb_ui_agent_chat.c` | Session Picker 全屏 overlay + 异步拉取 + 切换 |
| `bb_notification.c` | 通知 FIFO（16 条）+ local_home WS（`ws://<adapter>/ws`）+ ACK |
| `bb_chat_cache.c` | NVS 内 1.5KB 最近消息环（ADR-017） |
| `bb_device_monitor.c` | USB 截图 + 按键注入开发闭环（ADR-015） |
| `bb_ota.c` | OTA 升级（仅 cloud_saas 模式支持） |
| `bb_theme_buddy_*` / `bb_agent_theme.c` | 9 态 buddy 动画主题（ADR-009） |

分区：默认 `partitions_bbclaw.csv`（factory 3MB 无 OTA）；OTA 版 `boards/bbclaw/partitions_ota.csv`（factory 1MB + ota_0/ota_1 各 2.5MB + resources 1MB）。

### 3.2 Adapter — Go Agent Bus（`adapter/`，约 86 个 `.go`）

桌面常驻守护进程，是**协议的权威定义方**（WebSocket envelope、HTTP API 契约、音频流格式）。向上暴露统一 `AgentDriver` 接口，向下把帧转成各 CLI 的 stdin/stdout。

内部分层：Frame layer（解析/编码设备帧）→ Router layer（session ID ↔ driver 映射）→ Driver layer（每家 CLI 一个实现）。

5 个可插拔 driver（`adapter/internal/agent/`）：`claudecode`（多轮 JSONL session）、`opencode`、`ollama`、`aider`、`openclawdriver`。另有 `driverstate`（持久化 `~/.bbclaw-adapter/driver_state.json`）、`logicalsession`（ADR-014 逻辑 session 抽象）、`router.go`。

其他子系统：`asr/`、`tts/`、`audio/`、`pipeline/`、`homeadapter/`（Cloud relay WS 上行）、`httpapi/`（本地 `:18080`）、`openclaw/`、`voicecmd/`。

关键 HTTP 接口：`GET /v1/agent/sessions`、`POST /v1/agent/message`（NDJSON 流）、`GET /v1/agent/drivers`、`PUT /v1/agent/active_driver`、`PUT /v1/agent/drivers/{name}/active_model`、`GET /ws`、`DELETE /v1/agent/sessions/{id}`。

### 3.3 Cloud — Go 云控制面（`bbclaw-reference/cloud/`，约 69 个 `.go`）

设备与远端 Adapter 之间的中继 + 多租户控制面。监听 `:38081`（生产经 Nginx 反代到 `bbclaw.daboluo.cc`）。

子系统（`cloud/internal/`）：`httpapi/`（所有 HTTP 路由）、`router/`（WS hub，设备↔home_adapter 路由 + `pendingNotifs` 离线积压队列 32/设备）、`account/`+`device/`+`pairing/`（JSON 文件持久化）、`asr/`+`tts/`（可插拔 provider：doubao_native / local_command / local_http / openai_compatible）、`ota/`、`voiceprint/`、`event/`、`billing`/`preorder/`。

门户 API `/v1/portal/agent/*` 已实现：sessions CRUD、cwd-pool、drivers、messages、`POST /message`（流式 NDJSON）、`POST /sessions/{id}/approve`（工具批准）。

### 3.4 Web — React 用户门户（`bbclaw-reference/web/`，约 38 个 `.jsx/.js`）

React Router SPA：公开路由（`/`、`/features`、`/reserve`）+ 认证 app 路由（`/app/*`）。token 存 `localStorage` 的 `bbclaw.portal.token`。Web 与未来移动 App 同为 Cloud 的 SaaS 客户端，结构上与 ESP32 固件平行——都是让登录用户通过 Cloud relay 操作自己的本地 Adapter / agent。

`/app/tools/chat` 当前为 **Phase 1 LAN-only**（浏览器直连 Adapter `:18080`，绕过 Cloud），仅适合开发；Cloud-relay 模式（F9）门户路由已就位，ChatPage 仍需补一个 transport 开关——这是移动端对齐的下一步。

---

## 4. 协议契约（跨端同步要点）

Adapter 定义权威协议。改动协议级契约时，Cloud / Firmware 必须同步。

`session.notification` envelope（Adapter → Cloud / Device）：

```json
{
  "type": "event",
  "kind": "session.notification",
  "deviceId": "<device-id>",
  "payload": {
    "sessionId": "cc-xxx",
    "driver": "claude-code",
    "type": "turn_end",
    "preview": "PR #42 已创建",
    "timestamp": 1714700123000
  }
}
```

Cloud hub 路由规则：`session.notification` from adapter → 设备在线则转发，离线则 `enqueueNotification()` 积压；设备 `Register()` 时 flush `pendingNotifs[deviceId]`。

跨仓库同步检查清单：

| 变更 | 需同步检查 |
|------|-----------|
| 通知 payload 字段 | Adapter `notifications.go` + Cloud `hub.go` + Firmware `bb_notification.c` |
| 新增 agent proxy kind | Adapter `adapter.go:handleRequest` + `agent_proxy.go` + Cloud `agent_proxy.go` + `server.go` 路由 |
| Session API 变更 | Adapter `agent.go` + Cloud `agent_proxy.go` + Firmware `bb_agent_client.c` |
| WS envelope 格式 | Adapter `homeadapter/` + Cloud `router/hub.go` + Firmware `bb_adapter_client.c` |

> **已知缺口**：Cloud `POST /v1/portal/agent/sessions/{id}/approve` 代理到 hub kind `agent.sessions.approve`，但 Adapter `homeadapter/adapter.go:handleRequest` 尚无对应 case，approve 调用会返回 `HOME_ADAPTER_TIMEOUT` 直到补上该分支。

Cloud 错误信封（Web 消费）：`{"ok":false,"error":{"code":"HOME_ADAPTER_OFFLINE","detail":"..."}}`，已知 code：`HOME_ADAPTER_OFFLINE`(502)、`BINDING_REQUIRED`、`PAIRING_REQUIRED`、`DEVICE_NOT_FOUND`、通用 5xx。

---

## 5. 状态机设计

固件采用**多层独立状态机**，通过 `status` 字符串与回调联动：

- **PTT 业务态**（`bb_radio_app.c`）：`IDLE → CAPTURING`（PTT 按下录音）→ `WAITING`（松开发送、等响应）→ `IDLE`（响应完成）。`WAITING` 时按 PTT = 打断并重新录音。`session_busy` 标志贯穿整个录音-发送-等待周期。
- **App 锁状态**：`LOCKED / UNLOCKED`
- **UI 显示态**：`STANDBY / LOCKED / ACTIVE`
- **WiFi 连接态**：`NONE / STA_CONNECTED / AP_PROVISIONING`
- **LED 反馈态**：`IDLE / RECORDING / PROCESSING / REPLY …`
- **Agent 9 态状态机**（ADR-009）：在主题层驱动 SLEEP / IDLE / LISTENING / BUSY / SPEAKING / HEART / ATTENTION / CELEBRATE / DIZZY 等动画。

页面模型（ADR-012，2026-05-03 修订）：从早期 overlay 召唤模型重构为 **固定页面 + 显式状态机**，CHAT 成为主页（删除 STANDBY），Settings 为独立 `BBCLAW_STATE_SETTINGS` 页面，由用户 BACK 显式进入/退出。

---

## 6. 设计决策脉络（ADR 时间线）

`design/decisions/` 下 17 个 ADR，按时间线呈现架构演进：

| ADR | 标题 | 日期 | 状态 |
|-----|------|------|------|
| 001 | adapter 作为 Agent 总线（而非 Claude desktop BLE buddy） | 04-25 | 已接受 |
| 002 | 多轮会话生命周期（session 注册表 + sessionId 复用 + CLI `--resume`） | 04-25 | 已接受 |
| 003 | Router + 多 driver 路由策略（会话绑定 driver） | 04-25 | 已接受 |
| 004 | cloud_saas 模式下 Cloud 代理 `/v1/agent/*` 到 home adapter | 04-25 | 已接受 |
| 005 | openclaw 接入 AgentDriver（重评 ADR-001） | 04-26 | 已接受 |
| 006 | Flipper 6-button 完整事件 + LEFT/RIGHT 语义 | 04-26 | 已接受 |
| 007 | 独立 Settings overlay | 04-27 | 已替代 → 012 |
| 008 | Chat 作为待机首页 + 90s 空闲退出 | 04-27 | 已替代 → 012 |
| 009 | Agent 9 态状态机 LISTENING / BUSY / SPEAKING | 04-27 | 已接受 |
| 010 | Per-device AgentDriver 作为云配置 | 04-27 | 已接受 |
| 011 | Adapter 开源（搬到主仓） | 04-27 | 已接受 |
| 012 | 固定三页菜单取代 overlay 召唤模型 | 04-30 | 已接受 |
| 013 | 设备端会话历史回放与上下翻页 | 05-04 | 已接受 |
| 014 | Logical Session 抽象——把 CLI session 细节移出设备 | 05-04 | 已接受（待实现） |
| 015 | Device Monitor over USB（截图 + 按键注入） | 05-12 | 已实现 |
| 016 | 设备端 Driver / Model 选择（Settings 双行 + Adapter 持久化） | 05-17 | 已接受（已实现） |
| 017 | TTS 阅读模式 + Chat tail 缓存 | 05-18 | 已实现 |

**演进主线可读为三条：**

1. **Agent Bus 抽象逐步走穿**（001 → 002 → 003 → 005 → 010 → 014）：从单 driver 一次性请求，到多 driver 多轮 session，再到把 CLI 细节完全移出设备的逻辑 session 抽象。
2. **设备 UI 范式收敛**（006 → 007/008 被 012 替代 → 009 → 013 → 016 → 017）：从 overlay 召唤模型重构为固定页面 + 三态状态机，CHAT 升为主页，并补齐历史回放、driver/model 选择、TTS 阅读模式等 UX。
3. **工程基础设施**（004 cloud 代理、011 adapter 开源、015 USB 开发闭环）：让远端可用、让 adapter 可开源、让 AI 能自主跑「截图→改码→烧录→验证」闭环。

其他设计文档：`STATE_MACHINE.md`（状态机核心）、`agent_bus.md`（Agent 总线架构）、`firmware_agent_integration.md`（固件接入 Phase 4 蓝图）、`multi_session_management.md`（三端联动多 session）、`AGENT_STATE_MACHINE.md`、`device_config_sync.md`、`firmware_status_led.md`、`hardware_errata.md`、`release_verification.md`。

---

## 7. 工程与发布约定

- **设计文档优先**：`design/` 是开发决策唯一真相来源，代码与文档冲突时先解决设计。
- **单 tag 单 release 双产物**：推 `v*` tag 触发 `release.yml`，并行构建固件 `.bin` + 5 平台 adapter 二进制，发一个 GitHub Release 并把固件推到 OTA。固件与 adapter 作为协议配对协同发布。
- **何时打 tag**：固件有面向用户新特性，或 adapter 有修复/新特性。**不为** cloud-only / web-only 改动或纯内部重构打 tag。
- **版本约定**：每个有意义改动都 bump 版本 + 加 CHANGELOG，但仅在 adapter 需要发布时打 tag。
- **OTA**：仅 cloud_saas 模式支持，固件启动后 `GET /v1/ota/check` 查更新。
- **AI 烧录权限**（2026-05-12 起）：AI 可执行 `make build` / `make flash PORT=...` / `make boot-recover`，跑完整开发闭环；慎用 `make monitor`（前台阻塞）/ `make all`。

---

## 8. 设备信息速查

- 芯片：ESP32-S3（QFN56）rev v0.2，Flash 8MB
- USB Serial：`/dev/tty.usbmodem2112401`（烧录口 `/dev/cu.usbmodem2124401`）
- Chip MAC：`3c:84:27:c7:eb:88`
- 生产：`https://bbclaw.daboluo.cc/`（Nginx → 静态 dist + `127.0.0.1:38081` cloud systemd 服务）
- 代码情报：GitNexus 索引（主仓 bbclaw 77623 符号 / 130014 关系 / 281 执行流；reference 3714 符号）
