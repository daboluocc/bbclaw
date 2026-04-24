# ADR-001: adapter 作为 Agent 总线

- **编号**: ADR-001
- **标题**: adapter 作为 Agent 总线（而非 Claude desktop BLE buddy）
- **日期**: 2026-04-25
- **状态**: 已接受
- **相关文档**: [agent_bus.md](../agent_bus.md)

## 背景

bbclaw 需要让硬件设备和 AI agent 对话。当时摆在桌上的三条路：

1. **Claude desktop BLE buddy**：ESP32 直连 macOS/Windows 上的 Claude app，走官方 Nordic UART Service 协议（`claude-desktop-buddy` 参考项目）
2. **设备直连云 LLM**：固件内置多家 SDK + 鉴权，设备自己调 API
3. **adapter 做 Agent 总线**：桌面常驻 Go 程序 spawn 各家 CLI 作为子进程，用统一接口暴露给设备

评估过程中确认了路径 1 的关键限制（经查阅 `claude-desktop-buddy/REFERENCE.md` 全文 + 源码 grep）：

- 协议是**单向只读 + 审批回执**：设备只能读 Claude 输出（`heartbeat.msg`/`entries`/`turn` 事件）和回权限决定（`permission` 命令）
- **没有用户消息注入通道**：协议里不存在 `{"cmd":"user_msg"}` 这类帧，设备无法作为输入设备把文字推给 Claude 开启新一轮对话
- **仅限 developer mode**：需要用户开 `Help → Troubleshooting → Enable Developer Mode`，不是正式产品特性
- **Anthropic 专属**：协议是 Claude desktop app 定的，没法泛化到 Codex / Aider / Ollama
- **无音频通道**：协议只定义 JSON 控制帧，TTS/ASR 都不支持

路径 2 的问题：

- 每家 CLI 版本升级都要重烧固件
- 鉴权要塞进 ESP32，和用户桌面已有登录态重复
- ESP32 资源有限，多 SDK 并存难度大

## 决策

**采用路径 3：adapter 作为 Agent 总线。**

- 设备↔adapter 走自定义 JSON-line 帧（复用 buddy 风格降低心智成本，但扩展 `user_msg` / `switch_agent` / `session_new` 等设备主动帧）
- adapter 内部定义 `AgentDriver` 接口，每家 CLI 一个 driver 实现
- 每个 driver 声明 `Capabilities`（ToolApproval/Resume/Streaming/...），设备端据此调整 UX
- 会话历史归各 CLI 自己管（用 `--resume` 等），adapter 只存 session ID

详细接口、帧定义、能力矩阵、落地阶段见 [agent_bus.md](../agent_bus.md)。

## 后果

### 正向

- **可扩展**：加新 agent 只改 adapter，固件无需重烧
- **解耦**：不依赖 Anthropic 的 developer-mode BLE 协议，Claude desktop app 未来协议变更不影响 bbclaw
- **利用桌面登录态**：CLI 复用用户已有的 `~/.claude`、`~/.codex` 鉴权，不做鉴权代理
- **统一设备端 UX**：一套菜单/键位/屏幕布局对应多家 agent，切换零成本
- **音频可分离**：audio pipeline 和 agent bus 是两套通道，分别演进

### 负向

- **需要桌面常驻 adapter**：用户机器必须跑 adapter 进程；adapter 挂了设备就瞎
  - 缓解：adapter 做成 launchd/systemd 服务；设备检测到桌面离线有明确 UI 提示
- **CLI 输出格式耦合**：每家 CLI 的 stream 格式都要写一个 parser，upstream 改格式就要跟
  - 缓解：优先选有结构化输出的 CLI（`--output-format stream-json` 这种）；driver 分层隔离变化
- **不能完全替代 buddy**：那些只想在桌面旁边放个小挂件监视 Claude 的用户，adapter 方案重了
  - 缓解：未来不排除在 adapter 里再加一个"桥接 buddy BLE 协议"的 driver，让 bbclaw 同时能当 buddy 设备用（两种模式并存）

### 需要后续决策的开放问题

- session 生命周期跨 adapter 重启如何处理 → 新 ADR
- 是否支持多 session 并发 → 新 ADR
- 设备端历史缓存策略 → agent_bus.md §10
