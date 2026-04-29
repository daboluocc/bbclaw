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
