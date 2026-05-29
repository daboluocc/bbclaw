# ADR-019: Server-Driven 菜单协议 — 把设备 picker 渲染下沉到 adapter

- **日期**: 2026-05-30
- **状态**: 已接受（待实现）
- **关联**: ADR-012（固定页菜单）、ADR-013（历史回放）、ADR-014（逻辑会话）、ADR-016（设备端 driver/model 选择）、ADR-018（设备管家)、`docs/PROJECT_PROFILE.md` §4、`design/multi_session_management.md`

## 背景

### 产品定位（前提）

沿用 ADR-018:目标架构是**薄设备(哑终端) + 厚管家(adapter)**。设备只负责"录音 + 播报 + 渲染",业务状态与展示计算应在 adapter。

### 问题:四个 picker 在设备端各自造轮子

设备端 `bb_ui_agent_chat.c`(约 3178 行)为 session / cwd / driver / model 四个选择器各实现了一套 `异步 fetch → build_ui → apply_styles → select` 模板(合计上千行),并在设备端做了大量本属 adapter 的展示逻辑:

- session 列表的相对时间格式化(`format_relative_time`)、消息数/cwd 后缀拼装、active 标记;
- 历史分页游标(`history_min_seq`/`has_more`/`loaded_count`);
- driver 切换状态机(`cycle_driver` 与 `set_active_driver` 逐字重复)+ 第二份 `driver_cache`(`bb_ui_settings.c`)。

而 ADR-012 想消灭的"overlay 互斥矩阵"在 `bb_radio_app.c:1734-1818` 的 nav 路由里复活(手工 `if(cwd_up)/else if(picker_up)/else` 三层分发,全文件 35 处 `lvgl_port_lock`)。这是 ADR-018 诊断的"复杂度外溢到设备/协议层"的最直接体现。

ADR-018 P0之三 第一步(删 Phase 3-7 死骨架)已完成;本 ADR 定义第二步的协议契约。

## 决策

引入**统一的 server-driven 菜单协议**:adapter 把菜单**算好**(含预格式化的相对时间、预览、active 标记)以统一 JSON 下发;设备只保留**一个通用菜单渲染器**,显示行、移动光标、回选,零业务逻辑与零格式化。

### 1. Menu JSON(adapter → device)

```json
{
  "id": "sessions",
  "menuVersion": 1,
  "title": "会话",
  "selectedIndex": 0,
  "rows": [
    {
      "id": "ls-9d4f2a7c",
      "label": "Auth 重构",
      "secondary": "claude-code · 3 分钟前 · 12 条",
      "marker": "active",
      "action": { "type": "select_session", "sessionId": "ls-9d4f2a7c" }
    },
    {
      "id": "__new__",
      "label": "+ 新建会话",
      "action": { "type": "open_menu", "menuId": "cwd" }
    }
  ],
  "emptyText": "暂无会话"
}
```

- `label` / `secondary` / `marker` 全部由 adapter **预格式化**为可直接渲染的字符串(相对时间、消息数、active 标记等下沉);设备不解析、不计算。
- `marker`:`"active"`(持久的"当前选中项"指示)| `""`(默认)。光标位置由 `selectedIndex` 表达,与 marker 正交。
- `menuVersion`:协议版本,设备据此决定能否渲染;不认得的版本回退到旧 picker(迁移期)。
- 每行都带 `action`(闭集,见 §2);无 `action` 的行视为不可选(当前不产生)。

### 2. 动作集(闭集、可版本化)

| `action.type` | 字段 | adapter 行为 | action 响应 |
|---|---|---|---|
| `select_session` | sessionId | 设为该设备当前会话 | `{result:"closed", sessionId, loadHistory:true}` |
| `open_menu` | menuId | —(导航) | `{result:"navigate", nextMenu:{…}}` |
| `create_session` | cwd | mint 逻辑会话(ADR-014) | `{result:"closed", sessionId, loadHistory:true}` |
| `set_driver` | driver | 写 `driver_state.json`(ADR-016) | `{result:"refresh", nextMenu:{…}}` 或 `{result:"closed"}` |
| `set_model` | driver, model | 写 active_model(ADR-016) | `{result:"closed"}` |
| `close` | — | — | `{result:"closed"}` |

### 3. 端点(在现有 `/v1/agent/*` 上**新增**)

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/v1/agent/menu/{id}` | id ∈ `sessions`/`cwd`/`drivers`/`models`;返回 Menu JSON(`deviceId` 作 query) |
| `POST` | `/v1/agent/menu/action` | body `{deviceId, action}`;adapter 执行并返回 action 响应(见 §2) |

action 响应形:

```json
{ "result": "closed" | "navigate" | "refresh",
  "nextMenu": { /* Menu, 当 navigate/refresh */ },
  "sessionId": "ls-…",        /* select_session/create_session */
  "loadHistory": true }
```

cloud_saas 模式由 Cloud portal/relay 透传同两端点(见 §7)。

### 4. 设备渲染器(替代 4 个 picker)

新增单个 `bb_menu_view` 组件:
- 输入一段 Menu JSON → 渲染 `title` + 每行 `label`/`secondary`/`marker`;`rows` 为空时渲染 `emptyText`。
- **UP/DOWN**:移动 `selectedIndex`(clamp)。**OK**:对选中行 `POST /v1/agent/menu/action`,据响应关闭 / 压入 `nextMenu` / 刷新 / 触发历史加载。**BACK**:菜单栈深>1 则 pop,否则关闭返回 CHAT。LEFT/RIGHT 在菜单内不定义(保留 CHAT 内 driver cycle 语义,ADR-006/018)。
- 维护一个小**菜单栈**(`open_menu` 压入,BACK 弹出),取代 `bb_radio_app.c` 的手工三层互斥路由——nav 路由收敛成"菜单栈在不在顶"。

设备**不再**持有 `bb_chat_state_t` 里的 `session_list`/`cwd_pool`/`driver_cache` 状态机与四套 build_ui/select 逻辑。

### 5. 四个 picker 的映射

| 现有 picker | 菜单 id | 行 action |
|---|---|---|
| session picker | `sessions` | `select_session`;`+新建` 行 → `open_menu(cwd)` |
| cwd picker | `cwd` | `create_session{cwd}` |
| driver 切换 | `drivers` | `set_driver{driver}` |
| model 选择 | `models` | `set_model{driver, model}` |

### 6. 版本化与向后兼容

- menu 端点**纯新增**;现有 `GET /v1/agent/sessions`、`/cwd-pool`、`/drivers` 保留不动。
- 固件**逐个 picker 迁移**(先 driver/model 这类简单的,再 session/cwd),每迁一个就删 `bb_ui_agent_chat.c` 里对应的 build_ui/select/状态;迁移期两套并存,降低风险。
- `menuVersion` 让旧固件遇到不认得的菜单时回退旧 picker。

### 7. 跨仓同步(PROJECT_PROFILE §4)

协议级改动,三端必须同步:

| 端 | 改动 |
|---|---|
| **adapter**(权威) | `httpapi`:新增 menu 组装(复用 `logicalsession.List` / cwd-pool 配置 / `router.List` / `ListModels` / `driverstate`)+ 2 端点;`homeadapter`:cloud relay 同步两端点 |
| **cloud** | portal `/v1/portal/agent/menu/*` 代理 + hub kind 路由到 home adapter |
| **firmware** | 新增 `bb_menu_view` 渲染器;逐 picker 切换;简化 `bb_radio_app.c` nav 路由;删 `bb_ui_agent_chat.c` 对应逻辑 |

## 影响

### 正面
- 设备 `bb_ui_agent_chat.c` 上千行 picker 模板塌缩成一个渲染器;`bb_radio_app.c` 互斥路由大幅简化——直击 ADR-018 的"瘦身"诉求。
- 展示计算(相对时间/预览/标记)单点化在 adapter,跨 LAN/cloud 一致,且便于后续由"管家"按记忆/画像智能排序。
- 新增菜单类型(未来:project 画像、记忆条目)无需改固件,只加一个菜单 id。

### 负面 / Tradeoff
- 新增一套协议契约,三端同步成本(本 ADR 的迁移与版本化策略用以摊薄)。
- 每次开菜单多一次 `GET /v1/agent/menu/{id}` 往返(原本部分数据设备已缓存);可接受,菜单非高频且响应小。
- 设备渲染器需支持菜单栈,略增固件复杂度,但远小于删掉的四套 picker。

### 中性
- ADR-013 历史回放不变:`select_session` 响应带 `loadHistory` 提示,设备仍走原 messages 端点。
- ADR-016 的 driver/model 持久化语义不变,只是入口从设备本地菜单改为 menu action。

## 备选方案（已排除)
1. **仅重塑现有端点**(把格式化下沉进 sessions/cwd-pool/drivers 响应,不引入通用 menu 层):设备仍保留四套渲染/select 代码,瘦身不彻底,且每加一种菜单都要改固件。
2. **WS 推送菜单**(adapter 主动 push 菜单):菜单是用户拉起的、强请求-响应语义,pull 更自然;WS 留给通知(现状)。
3. **嵌进现有 `agent.event` NDJSON 流**:菜单与对话 turn 是不同交互,混在一个流里增加设备状态机复杂度。

## 实现 checklist

- [x] adapter: `httpapi` 菜单组装 + `GET /v1/agent/menu/{id}` + `POST /v1/agent/menu/action`(drivers/models/sessions/cwd 全部)
- [x] adapter: 共享 `internal/agent/menu` 包(类型+纯 builder),httpapi 与 homeadapter 复用,菜单形态两端一致
- [x] adapter: `homeadapter` cloud relay —— `agent.menu` / `agent.menu.action` 两个 kind
- [x] adapter: 单测(每种菜单的行/marker/action 组装;httpapi handler;homeadapter envelope 分发)
- [x] cloud: device-facing `GET/POST /v1/agent/menu/*` + portal `GET/POST /v1/portal/agent/menu/*` 代理到 hub kind(共享 proxyAgentMenu/Action)
- [ ] firmware: `bb_menu_view` 通用渲染器 + 菜单栈
- [ ] firmware: 逐 picker 迁移(drivers/models → cwd → sessions),删 `bb_ui_agent_chat.c` 对应逻辑 + 简化 `bb_radio_app.c` nav
- [ ] design: 更新 `multi_session_management.md`

## 跨层一致性验证(2026-05-30,对抗式)

对 4 条路径(LAN httpapi / homeadapter cloud-relay / cloud device-facing / cloud portal)做了对抗式核对,结论 **minor_issues**:

- ✅ 错误码→HTTP 状态:14 个 code 在 LAN 直写状态与 cloud `menuErrorStatus` 映射**逐一一致**。
- ✅ 菜单/结果 JSON:`menuToPayload` 的 round-trip 不丢/不增字段(仅对象键序不同,解析器无关);早先担心的 omitempty 单边丢字段**经核对不成立**。
- ✅ action 语义:6 个 action 在 LAN 与 home 的校验顺序/副作用/Result **逐一一致**。
- ✅ 路由/kind:cloud 发的 `agent.menu`/`agent.menu.action` 与 home `handleRequest` case 字符串完全匹配;device-facing 与 portal 均覆盖。
- ✅ **已修**:空 action 错误码分叉(LAN 曾返回 `UNSUPPORTED_ACTION`,cloud 返回 `EMPTY_ACTION`)→ 两侧统一为 `EMPTY_ACTION`。

**已知预存差异(非本协议引入,文档存档,后续单独处理):**
1. **错误信封形态**:LAN `response.error` 是 string + 平级 `detail`;cloud `response.error` 是 `{code,detail}` 对象。这是**全项目** device↔cloud 约定差异(drivers/sessions 等所有端点皆如此),菜单端点只是各自 follow 本侧约定;统一需改两仓的 `response` 类型,超出 ADR-019 范围。
2. **sessions 过期过滤**:LAN 菜单按 `SessionMaxAge` 过滤过期会话,cloud-relay 不过滤(`homeadapter.Config` 无该字段)——继承自既有 `sessions` list 端点的同款差异;`logicalsession.Sweep` 终会清除,影响小。修法:给 `homeadapter.Config` 加 `SessionMaxAge`。
