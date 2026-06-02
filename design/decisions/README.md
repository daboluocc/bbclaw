# 设计决策记录 (ADR)

记录重要的架构和设计决策。

## 记录格式

每个决策应包含：
- **编号**: ADR-XXX
- **标题**: 简洁描述
- **日期**: YYYY-MM-DD
- **状态**: 已接受 / 已废弃 / 已替代
- **背景**: 决策前的状况
- **决策**: 具体的决策内容
- **后果**: 采用后的影响

## 决策列表

| 编号 | 标题 | 日期 | 状态 |
|------|------|------|------|
| [ADR-001](ADR-001-adapter-as-agent-bus.md) | adapter 作为 Agent 总线 | 2026-04-25 | 已接受 |
| [ADR-002](ADR-002-multi-turn-session-lifecycle.md) | 多轮会话生命周期 | 2026-04-25 | 已接受 |
| [ADR-003](ADR-003-router-and-multi-driver.md) | Router + 多 driver 路由策略 | 2026-04-25 | 已接受 |
| [ADR-004](ADR-004-cloud-agent-proxy.md) | cloud_saas 模式下的 Agent Bus 代理 | 2026-04-25 | 已接受 |
| [ADR-005](ADR-005-openclaw-as-driver.md) | openclaw 接入 AgentDriver（重评 ADR-001） | 2026-04-26 | 已接受 |
| [ADR-006](ADR-006-flipper-full-nav-events.md) | Flipper 6-button 完整事件 + LEFT/RIGHT 语义 | 2026-04-26 | 已接受 |
| [ADR-007](ADR-007-standalone-settings-overlay.md) | 独立 Settings overlay（Phase 4.7） | 2026-04-27 | 已替代（→ ADR-012） |
| [ADR-008](ADR-008-chat-as-standby-and-idle-exit.md) | Chat 作为待机首页 + 90s 空闲退出（Phase 4.8.x） | 2026-04-27 | 已替代（→ ADR-012） |
| [ADR-009](ADR-009-agent-state-machine.md) | Agent 9 态状态机：LISTENING / BUSY / SPEAKING（Phase 4.8.x） | 2026-04-27 | 已接受 |
| [ADR-010](ADR-010-per-device-agentdriver-cloud-config.md) | Per-device AgentDriver 作为云配置（v0.4.0 多 driver） | 2026-04-27 | 已接受 |
| [ADR-011](ADR-011-adapter-open-source.md) | Adapter 开源（搬到主仓） | 2026-04-27 | 已接受 |
| [ADR-012](ADR-012-fixed-page-menu.md) | 固定三页菜单（Standby / Chat / Settings）取代 overlay 召唤 | 2026-04-30 | 已接受 |
| [ADR-013](ADR-013-session-history-replay.md) | 设备端会话历史回放与上下翻页 | 2026-05-04 | 已接受 |
| [ADR-014](ADR-014-logical-session-abstraction.md) | Logical Session 抽象——把 CLI session 细节移出设备 | 2026-05-04 | 已接受 |
| [ADR-015](ADR-015-device-monitor-over-usb.md) | Device Monitor over USB（截图 + 按键注入） | 2026-05-12 | 已实现 |
| [ADR-016](ADR-016-device-driver-model-selection.md) | 设备端 Driver / Model 选择（Settings 双行 + Adapter 持久化） | 2026-05-17 | 已接受（已实现） |
| [ADR-017](ADR-017-tts-reading-mode-and-chat-cache.md) | TTS 阅读模式 + Chat tail 缓存 | 2026-05-18 | 已实现 |
| [ADR-018](ADR-018-device-butler-architecture.md) | 设备管家(Butler)——Adapter 作为会话编排 + 记忆 + Claude 完整适配中枢 | 2026-05-30 | 已接受（实施中） |
| [ADR-019](ADR-019-server-driven-menu-protocol.md) | Server-Driven 菜单协议——把设备 picker 渲染下沉到 adapter | 2026-05-30 | 已接受（待实现） |
| [ADR-020](ADR-020-memory-pipeline.md) | 记忆管线——用户需求记忆 + 本地项目画像（复用 Claude 原生） | 2026-05-30 | 草案（v1 范围明确，蒸馏延后） |
| [ADR-021](ADR-021-conversational-orchestrator-butler.md) | 对话式编排管家——Claude 管家会话 + MCP 派发到 worker CLI Agent | 2026-05-30 | 草案（方向已定，2 个 spike 闸门） |
| [ADR-022](ADR-022-memory-consolidation.md) | 记忆沉淀引擎——收件箱归档进 MEMORY 多维画像并清空 | 2026-06-02 | 已接受（v1，LOCAL-only，默认关灰度） |
