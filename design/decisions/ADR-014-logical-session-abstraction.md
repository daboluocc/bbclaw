# ADR-014: Logical Session 抽象 — 把 CLI session 细节移出设备

- **日期**: 2026-05-04
- **状态**: 已接受（待实现）
- **关联**: ADR-001（Adapter as Agent Bus）、ADR-002（Multi-turn session lifecycle）、ADR-003（Router + multi-driver）、ADR-013（Session history replay）、`design/multi_session_management.md`

## 背景

### 产品定位（前提）

BBClaw 是 **Claude Code / OpenCode / Aider 等 Agent CLI 的语音外设**，不是这些 CLI 的替代品。
形态接近 Siri / Alexa：用户从不关心"我后台是哪一个 conversation"，只关心"我和我的 BBClaw
设备一直在聊"。所有 CLI 实现细节（`cwd`、CLI session id、`--resume` 行为）应当对设备
**完全透明**。

### 现有设计的抽象泄漏

S1/S2 阶段（commit `9ea4c4d`）+ ADR-013 让设备直接持有 claude-code 的原始 session id：

- firmware NVS 存 `agent/session/<driver>` = CLI 自家分配的 conversation UUID
  （比如 `8a352891-4f65-4fad-a04e-e8cfa8fc21d6`）
- 设备 picker 列出 adapter 直接转发的 CLI session 列表
- 每次 turn firmware 把这个 id 透传给 adapter，adapter 再 `claude --resume <id>`

这个模型有三个问题：

1. **CLI session 跟 `cwd` 强耦合**：claude-code 把 conversation 存在
   `~/.claude/projects/{cwdHash}/{sid}.jsonl`。同一个 session id 离开它原来的 `cwd`
   就找不到。设备不可能也不应该知道 `cwd`。
2. **CLI session 会消失**：用户清 `~/.claude/projects/`、跨机器迁移、或 CLI 自己 GC，
   都会让设备持有的 session id orphan。复现实例（adapter log 2026-05-04T13:41:59）：
   ```
   WARN claude-code stderr sid=8a352891-...:
     No conversation found with session ID: 8a352891-4f65-4fad-a04e-e8cfa8fc21d6
   ```
   设备只能给用户一个 `AGENT_TURN_FAILED`，整个 turn 报废。
3. **设备端无法"新开 session"**：因为新 session 必须绑一个 `cwd`，但设备没有文件浏览器、
   没有键盘、也不在主机上 — 选 cwd 是个无解的 UI 命题。结果是设备只能用旧的 session，
   旧 session 一坏就废，体验脆弱。

## 决策

引入 **logical session** 概念，把 CLI 实现细节统一上移到 adapter（local_home 模式）和 cloud
控制台（cloud_saas 模式）。设备端只认 logical session id，永远稳定，与 CLI 实现解耦。

### 1. 概念模型

```
firmware                      adapter / cloud                  CLI (claude-code 等)
─────────                     ───────────────                  ──────────────────
device_id          ──────►    LogicalSession                   conversation
                                 ├─ id (稳定)
                                 ├─ device_id
                                 ├─ cwd (来自配置)
                                 ├─ title
                                 ├─ cli_session_id    ──────►  jsonl in
                                 │  (可被替换)                  ~/.claude/projects/
                                 └─ last_used_at
```

- `LogicalSession.id` 是 BBClaw 自己 mint 的稳定 id（建议 `ls-<uuid>` 前缀，避免和 CLI 自家 id 混淆）
- `cli_session_id` 失效时由 adapter **透明替换**，logical id 不变，设备无感
- `cwd` 由用户在 adapter 配置 / cloud 控制台预先指定，**设备 UI 永远不出现 cwd 选择**

### 2. Adapter 端职责

- **持久化映射**：本地存储 `~/.bbclaw-adapter/sessions.json`（或 SQLite，取决于规模），
  schema：
  ```json
  {
    "device_id": "BBClaw-0.4.1-C7EB89",
    "logical_sessions": [
      {
        "id": "ls-9d4f2a7c",
        "driver": "claude-code",
        "cwd": "/Users/mikas/code/myproject",
        "cli_session_id": "8a352891-4f65-4fad-a04e-e8cfa8fc21d6",
        "title": "Refactor auth module",
        "created_at": "2026-05-04T10:00:00Z",
        "last_used_at": "2026-05-04T13:42:00Z"
      }
    ]
  }
  ```
- **配置 cwd 池**：adapter `.env` 加 `BBCLAW_DEFAULT_CWD=/path/to/project`，
  以及可选的命名 cwd 池 `BBCLAW_CWD_POOL_<NAME>=/path/...`（多项目场景）。
  无配置时 fallback 到 adapter 启动 cwd。
- **失效自动恢复**：claude-code stderr 出现 `No conversation found with session ID` 时
  （现在已经识别为字符串 — 见 `adapter/internal/agent/claudecode/driver.go:209` 的
  SESSION_BUSY 同址）：
  1. **不**报错给设备
  2. 用同一 `LogicalSession.cwd` 重 spawn claude-code（不带 `--resume`）
  3. 等 `claude-code: session init` 事件拿到新的 `cli_session_id`
  4. 写回 `LogicalSession.cli_session_id`
  5. 把当次用户 transcript 转给新进程 stdin
  6. 流式回 reply，设备完全感知不到底层换 conversation
- **新建 session API**：`POST /v1/agent/sessions` 接受
  `{driver, cwd_name?, title?}`，分配新 logical id，**不立即 spawn CLI**
  （延迟到第一条 message 到达，省资源）。
- **列表 / 标题 API**：现有 `GET /v1/agent/sessions` 返回 logical sessions，
  `cli_session_id` 不下发给设备；adapter 内部使用。

### 3. Cloud 控制台职责（cloud_saas 模式）

- Web portal 增加"BBClaw 设备 → Sessions"管理界面：
  - 列表：每条 logical session 的 title / cwd / 最近使用时间
  - 操作：编辑 title、改 cwd（重绑定到不同项目）、删除、标记 archived
  - **新建**：选 cwd（来自 adapter 上报的 cwd 池）+ 输入 title，cloud 创建 logical
    session 并下发到 adapter 持久化
- Cloud 不直接持有 `cli_session_id`，仍由 adapter 维护；cloud 只维护 logical 元数据
  + 路由信息

### 4. Firmware 端简化

| 之前 | 之后 |
|------|------|
| NVS 存 `agent/session/<driver>` = CLI conversation UUID | NVS 存 `agent/lsession/<driver>` = logical session id |
| Session picker 列 CLI session 列表 | Session picker 列 logical session（title 是用户在控制台设的） |
| "新建 session" 不可用（无法选 cwd） | "新建 session" = `POST /v1/agent/sessions`，**无 cwd 参数**，adapter 用 default cwd |
| CLI session id 失效 → AGENT_TURN_FAILED → 用户卡死 | CLI session id 失效 → adapter 透明换底，设备无感 |

设备端 NVS 字段建议改名（`agent/session` → `agent/lsession`），首次启动时如果发现旧字段
存在，**清空即可**（不做迁移，旧 cli session 反正大概率已失效）。

### 5. 协议变更

| Endpoint | 之前 | 之后 |
|----------|------|------|
| `POST /v1/agent/message` | `body.sessionId` = CLI session id（可空） | `body.sessionId` = logical session id（可空，空则用设备默认） |
| `GET /v1/agent/sessions` | 返回 CLI session 元数据 | 返回 logical session（title 来自控制台） |
| `GET /v1/agent/sessions/{id}/messages` | id 是 CLI session id | id 是 logical session id；adapter 内部转 cli session id 后查 jsonl |
| `POST /v1/agent/sessions` | 不存在 | **新增**：mint 新 logical session（设备端"新建"按钮触发） |
| `DELETE /v1/agent/sessions/{id}` | 不存在 | **新增**：删 logical session（cli conversation 文件可保留也可同删，配置决定） |

### 6. 向后兼容

为了不在一个 PR 里改三层，分阶段：

- **Phase A** (adapter 单独发版)：adapter 同时接受
  1. 旧 firmware 直发 cli session id（按当前路径处理）
  2. 新 firmware 发 logical id（走新映射路径）
  靠 id 前缀区分（logical id 强制 `ls-` 前缀）。同时实现失效自动恢复 — 这一项**对旧
  firmware 也立即生效**（旧 firmware 持有的 cli id 失效时 adapter 透明 mint 新的、
  存进新 logical 表，旧 firmware 下次还能继续用）。
- **Phase B** (firmware v0.5)：firmware NVS 字段改 `agent/lsession`，picker 改对接
  logical sessions API，新增"新建 session"入口。
- **Phase C** (firmware v0.6+)：adapter 移除对裸 cli session id 的接受，强制 logical。

## 影响

### 正面

- 设备端体验从"对话偶发丢失"变成"永远在线"
- firmware 移除一段 hard-to-justify 的 NVS 状态（cli session id 持久化）
- 新增 session 这个 UX 终于可以提供给设备用户
- 跨设备 / 跨机器迁移 / claude-code 升级 — 这些原本会让 cli session 失效的事情，全部对设备透明
- 用户可以在 web portal 给 session 起人话标题（"Auth 重构"、"周末 side project"），picker 显示远比 UUID 友好

### 负面 / Tradeoff

- adapter 状态变重：原本是无状态进程，现在要持久化一个 sessions.json（或类似存储）。失败模式
  增加（文件损坏 / 升级 schema migration）。代价 acceptable，因为 adapter 反正长期常驻。
- "切项目" 这个能力从设备消失 — 必须通过 adapter 配置或 web portal 切。**这是有意为之**：
  和 BBClaw 产品定位一致（语音外设 / 给单个项目配的智能麦），不是通用语音助手。
- cloud_saas 模式下 web portal 多一个管理面，前端工作量。

### 中性

- ADR-013 的 history replay 路径不变 — 只是 endpoint 里的 id 语义换成 logical，
  adapter 内部多一步映射查询。
- ADR-002 的 multi-turn 生命周期不变。

## 备选方案（已排除）

1. **设备端新建 session 时让用户选 cwd**：违反产品定位，设备 UI 物理上做不到
2. **设备端新建 session 自动用 device_id 当 cwd**：cwd 必须是真实存在的文件系统路径，
   device_id 不是
3. **cloud 维护历史，每次拼 prompt 给 stateless claude-code**：失去 prompt cache，
   token 成本高，长对话上下文崩盘
4. **claude-code 失效就让用户重启 / 在控制台手动修**：违反"设备永远在线"的定位

## 实现 checklist

- [ ] adapter: `internal/agent/logicalsession/` package（持久化 + 映射）
- [ ] adapter: `claudecode/driver.go` 失效检测 + 透明 retry
- [ ] adapter: `httpapi/agent.go` `POST /v1/agent/sessions` endpoint
- [ ] adapter: 现有 endpoints 加 logical id 路由层
- [ ] cloud: web portal sessions 管理页
- [ ] firmware (v0.5): NVS 字段改名 + picker 切 logical id + 新建 session UI
- [ ] firmware (v0.5): 启动清理旧 NVS 字段
- [ ] design: 同步更新 `multi_session_management.md`（标记 ADR-014 supersedes）
