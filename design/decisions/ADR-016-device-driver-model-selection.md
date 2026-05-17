# ADR-016: 设备端 Driver / Model 选择 — Settings 双行 + Adapter 持久化

- **日期**: 2026-05-17
- **状态**: 已接受（已实现）
- **关联**: ADR-001（Adapter as Agent Bus）、ADR-003（Router + multi-driver）、ADR-004（Cloud agent proxy）、ADR-010（Per-device agent driver cloud config）、ADR-014（Logical session）

## 背景

ADR-003 让 adapter 同时管 N 个 driver（claude-code / opencode / aider / openclaw / ollama），但**哪个是当前 driver**直到本 ADR 之前只能通过：

- 启动期 `AGENT_DEFAULT_DRIVER` 环境变量（操作员侧，user 改不了）
- 每次 `POST /v1/agent/message` 的 `driver` 字段（程序化调用者侧）

设备没有任何 UI 入口能切换 driver。"Model" 这个概念在 firmware 完全不存在 —— claude-code 永远跑操作员通过 `AGENT_CLAUDE_CODE_EXTRA_ARGS` 配的那个模型，要换 Opus → Sonnet 必须 SSH 进 adapter 主机改 env 重启。

用户场景：

- 早上想用 Sonnet 4.6 写代码；下午想用 Haiku 跑些小问题省 token —— 想直接在 BBClaw 上切。
- 同事过来想试 Ollama 本地模型 —— 直接旋钮选一下，不该要回 PC 操作。
- 多设备共享一个 adapter（家里多个 BBClaw）—— 改默认 model 应该所有设备同步看到。

## 决策

### 1. UI 形态（二级菜单 + Session Picker 去 Driver 行）

**硬件输入约束**（修订）：BBClaw 实际只有旋钮 ↑/↓ + OK + BACK。代码里的 `BB_NAV_EVENT_LEFT/RIGHT` 是早期硬件残留，运行时不会触发。这排除了最初设计的"扁平 + LEFT/RIGHT 循环值"模式（首版方案，2026-05-17 用户实测后修订）。

**Settings 屏（chat 长按 OK 入口）— 二级菜单**：

主屏：

```
Settings
  Driver: Claude Code         ← OK 进 Driver picker
  Model:  Sonnet 4.6          ← OK 进 Model picker
  TTS:    On                  ← OK 就地 toggle（二态用不着子屏）
  Back                        ← OK 退到 chat
```

子屏（Driver / Model picker 同构）：

```
Driver
  Claude Code  *              (* = adapter 持久化的 active)
  OpenCode
  OpenClaw
  Ollama
```

- 旋钮 ↑/↓：移光标
- 子屏 OK：commit（async PUT 到 adapter）+ 回主屏
- 子屏 BACK：放弃 + 回主屏
- 主屏 BACK / Back 行 OK：退到 chat

**Session Picker（chat 短按 OK 入口）— 去 Driver 行**：

```
Sessions (claude-code) 1/3      ← 标题只读显示当前 driver
+ 新建 session
ls-2025-05-17 14:32  <
ls-2025-05-16 09:15
ls-2025-05-15 11:00
```

切 driver **只在 Settings 里**——重复彻底消除。Driver 是低频操作（用户原话："以前要 SSH 改 env"），放低频路径（长按 OK）合理；Session 是每天多次的高频操作，短按 OK 直达保留。

**Model picker 的特殊情况**：当前 driver 不支持 model（`openclaw`）—— picker 显示 `(no models)`，OK 是 no-op；主屏 Model 行显示 `Model: (n/a)`。

**修订之前的扁平方案被淘汰的原因**：原计划是 5 行 `Driver / Model / Session / TTS / Back`，每行用 LEFT/RIGHT 循环值 + OK 提交。设备实际烧固件后用户报告无法切 model，定位发现没有 LEFT/RIGHT 物理键。改为二级菜单后没有"预览值"概念，旋钮 + OK + BACK 三键即够。

### 2. 持久化归属 — Adapter 侧

`active_driver` 和每个 driver 的 `active_model` **都存在 adapter** 上，文件 `BBCLAW_DATA_DIR/driver_state.json`（默认 `~/.bbclaw-adapter/driver_state.json`），格式：

```json
{
  "active_driver": "claude-code",
  "active_models": {
    "claude-code": "claude-sonnet-4-6",
    "ollama": "llama3.1:8b"
  }
}
```

设计权衡：

| 候选 | 选择 | 理由 |
|---|---|---|
| Adapter 侧（选） | ✓ | 一个 adapter 服务多个 BBClaw 时所有设备同步看到一致状态；cloud_saas 模式下与 cloud 端 admin console 看到的一致 |
| 设备 NVS 侧 | ✗ | 多设备各自独立，会出现"客厅那台用 Sonnet，卧室那台还是 Opus"的迷惑 |
| 混合（adapter 默认 + device override） | ✗ | 实现复杂、用户心智模型复杂、几乎没有真实需求 |

设备端**只缓存** `drv/active`（NVS key），用途是"adapter 不可达时先用上次的 driver 名启动 chat"，不是真相来源。

### 3. Driver 接口 — 可选的 `ModelLister`

```go
type ModelInfo struct {
    ID    string `json:"id"`              // 传给 driver 的 stable id
    Label string `json:"label,omitempty"` // 设备 UI 显示用
}

type ModelLister interface {
    ListModels(ctx context.Context) ([]ModelInfo, error)
}
```

可选接口模式（参考已有 `SessionLister`、`MessageLoader`、`CLISessionChecker`）—— 实现的 driver 暴露 model 列表，不实现的让 Settings UI 隐藏 Model 行。

各 driver 列表来源：

- **claude-code / opencode / aider**：静态硬编码列表（`<driver>/models.go`）。Anthropic / OpenAI / 其他 provider 没有稳定可机读的模型清单端点；手维护一行加一个 model。
- **ollama**：调用本地 `GET /api/tags`，60s 内 LRU 缓存，2s 超时；失败回落到上次成功的快照或空列表。
- **openclaw**：不实现接口（model 由 gateway 服务端决定，设备侧不该有选择权）。

### 4. Model 注入到 session

新增 `agent.StartOpts.Model string`。Driver `Start()` 时存到 session struct，`Send()` 时按 CLI 自家约定使用：

- claudecode: 在 `--resume` / `--session-id` 之后、`d.extra` 之前追加 `--model <id>`
- opencode: 同上
- aider: 在固定 args 之后追加 `--model <id>`
- ollama: 写到 `/api/chat` 请求体 `model` 字段；空时回落到 `d.model`（来自 New Options）

HTTP / cloud-proxy 层从 `driverState.ActiveModel(drv.Name())` 取值填进 StartOpts。

### 5. HTTP API（local_home + cloud_saas 共用）

| 端点 | 行为 |
|---|---|
| `GET /v1/agent/drivers` | 扩展返回：`{active_driver, drivers:[{name, capabilities, models[], active_model}]}` |
| `PUT /v1/agent/active_driver` | body `{"name":"..."}`；同步更新 `router.SetDefault()` |
| `PUT /v1/agent/drivers/{name}/active_model` | body `{"model":"..."}`；`model=""` 清除 override |

新字段都是**累加**的 —— 老固件读旧字段不受影响，老 cloud relay 透传 `agent.drivers` envelope 不变。

### 6. Cloud relay envelope 镜像

ADR-004 的 cloud 端通过 WS envelope `kind` 转发 firmware HTTP 调用。本 ADR 加 / 扩这几个 kind：

- `agent.drivers` —— 扩展 reply payload，加 `active_driver` 顶层字段、每个 driver 行加 `models` 与 `active_model`
- `agent.active_driver.set` （新）—— 镜像 `PUT /v1/agent/active_driver`
- `agent.active_model.set` （新）—— 镜像 `PUT /v1/agent/drivers/{name}/active_model`

**Cloud 端配套工作**（本 repo 不含）：cloud relay 需要 HTTP-proxy 上述 PUT 路径到对应 envelope kind，对应的 reply 翻译回 HTTP 响应。新字段透明转发即可。**未做这一步时 cloud_saas 模式下 Settings 修改会失败（500），但 local_home 模式立即可用**。

### 7. 双模式同一码路径

Firmware 在两种模式下都调相同 HTTP 路径（`agent_build_url`），只是 base URL 不同（local 模式直连 adapter，cloud_saas 模式经 cloud relay）。Adapter 侧 HTTP handler 与 cloud envelope handler 都从同一 `driverstate.Store` 读写，**一致性靠共享状态保证**而非协议双跑。

### 8. 失败模式 / 降级

| 场景 | 行为 |
|---|---|
| Adapter 没启 driverstate（无家目录） | Settings 显示真实 driver 列表但 PUT 返回 501 `DRIVERSTATE_NOT_CONFIGURED`，UI 提交悄默失败、不破坏 chat 流 |
| 设备开启 Settings 时 fetch 超时 | 显示 `Driver: (offline)`，Model / Session 行 `(n/a)` / `(none)`，仍可退出 |
| 持久化的 driver 在 adapter 重启后不再注册（操作员从 `AGENT_ENABLED_DRIVERS` 移除） | adapter log warn + router 用 first-registered 兜底；下次 Settings 进入时 UI 会发现 active_driver 不在列表里、归 0 |
| 持久化的 model id 不在当前 ListModels 列表里 | adapter **不拒绝**——把 id 原样塞给 CLI（让 CLI 自己 reject）。允许操作员配只在 OLLAMA_BASE_URL 远端可见的 model |

## 实现要点

### Adapter

- `adapter/internal/agent/driverstate/store.go`：文件后端、读写均加锁、写 tmp+rename 原子写
- `adapter/internal/agent/{claudecode,opencode,aider,ollama}/models.go`：实现 ModelLister
- `adapter/internal/agent/{claudecode,opencode,aider,ollama}/driver.go`：session 增加 `model` 字段，Send 时使用
- `adapter/internal/httpapi/agent.go`：扩展 handleAgentDrivers，新增两个 PUT handler，CORS allow PUT
- `adapter/internal/homeadapter/agent_proxy.go`：扩展 handleAgentDriversRequest + 加两个 set handler
- `adapter/internal/homeadapter/adapter.go`：dispatch 加两个 kind + SetDriverState
- `adapter/cmd/bbclaw-adapter/main.go`：buildDriverState + applyDriverStateDefault

### Firmware

- `firmware/include/bb_agent_client.h`：扩展 `bb_agent_driver_info_t`（含 models + active_model）；改 `bb_agent_list_drivers` 签名（+ active_driver out）；新增 `bb_agent_set_active_driver` / `bb_agent_set_active_model`
- `firmware/src/bb_agent_client.c`：实现两个新 PUT + 解析 models / active_driver
- `firmware/include/bb_session_store.h`、`firmware/src/bb_session_store.c`：新增 `drv/active` NVS key + load/save 函数（deferred persist 同既有模式）
- `firmware/src/bb_ui_agent_chat.c`：driver fallback 改读 NVS `drv/active`
- `firmware/src/bb_ui_settings.c`：完全重写。加 ROW_DRIVER + ROW_MODEL，driver 缓存 + 模型联动 + async commit 任务

## Driver ↔ Session ↔ Model 关系（修订 2026-05-17）

```
adapter ─┬─ driver_state.json          # 单一真相源
         │   active_driver
         │   active_models[driver]
         │
         └─ sessions.json
             ls-... → {driver, cwd, cli_session_id, ...}

firmware NVS（device-local cache，重启不丢；adapter 不可达时兜底）
  drv/active                            # 上次选的 active driver
  ls/cc, ls/oc, ls/op, ls/ol, ls/ai     # 各 driver 上次用的 logical session id
```

**强不变量**：
- 一个 logical session 永远绑一个 driver（adapter sessions.json 字段，不可改）
- 一个 driver 可有多个 logical session，但同时只激活一个（NVS `ls/<driver>` 记录）
- 切 driver = 切到那个 driver 上次的 session（NVS 读 `ls/<new driver>`，可能为空 → 自动 mint）

**切操作的级联**：

| 触发 | 顺序 |
|---|---|
| Settings 选 Driver, OK | PUT `/v1/agent/active_driver` → NVS `drv/active` → `bb_ui_agent_chat_set_active_driver(new)` → 读 `ls/<new>` → swap chat UI + 拉历史 |
| Settings 选 Model, OK | PUT `/v1/agent/drivers/{name}/active_model` → `bb_ui_agent_chat_set_active_model(label)` → bottom bar 显示更新 |
| Session Picker 选 sess | 写 `ls/<current driver>` → swap chat UI + 拉历史 |
| Session Picker 选 `+ New` | POST `/v1/agent/sessions` → 拿新 logical id → 写 `ls/<current driver>` |
| chat 长按 OK | 进 Settings 屏（chat 状态不变） |

## UI 元素（修订 2026-05-17）

```
┌─────────────────────────────────────────┐
│ status driver_label [buddy face/mood] 🔋│  ← topbar (bb_lvgl_display)
├─────────────────────────────────────────┤
│                                         │
│  ...transcript...                       │  ← 中间区域 (theme buddy-anim)
│                                         │
├─────────────────────────────────────────┤
│ sid abc12345           Sonnet 4.6       │  ← bottom bar (bb_lvgl_display)
└─────────────────────────────────────────┘
```

底栏右半从 `cwd_name` 改为 **active model**（ADR-016 第二轮修订）。理由：
- cwd 是 adapter 一次性配置，用户不会频繁切；运行时不显眼无所谓
- model 是日常会切的操作，必须一眼可见
- cwd 信息仍在 driver_cache 里保留，未来可加在 Settings 子屏作为附加信息

cwd_name 显示功能保留了 setter (`bb_display_set_cwd_name`),只是不再绘制；现有调用点不变,需要时可一行 patch 重新启用。

## 未做 / 已知遗留

- **Warm pool × model 切换**：claudecode warm pool 预热的 session 是用启动期 `--model` 参数 spawn 的；用户切到 Opus 之后，已经预热的池条目仍是 Sonnet。当前接受这个轻微不一致（被 acquire 后再 Send 时会再 pass 新 `--model`，但 CLI 是否真的覆盖未深入测试）。需要时可通过"切 model 时 Drain pool"补救。
- **Cloud 侧实现**：本 ADR 落地后，cloud 端要补 HTTP proxy + envelope handler 才能让 cloud_saas 模式下 Settings 真正生效。这是非破坏性变更（新增路由），可独立部署。
- **Web admin 界面**：cloud 侧 web 控制台暂未给 active_driver / active_model 加可视化编辑入口。

## 参考

- 实现：commit pending
- 相关讨论：见会话 2026-05-17
