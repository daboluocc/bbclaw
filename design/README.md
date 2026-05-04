# 设计文档（Source of Truth）

BBClaw 项目的系统级设计文档，是开发决策的唯一真相来源。

## 产品定位

BBClaw 是 **Claude Code / OpenCode / Aider 等 Agent CLI 的语音外设**，不是这些 CLI 的替代品。
形态接近 Siri / Alexa：用户从不关心"我后台是哪一个 conversation"，只关心"我和我的 BBClaw
设备一直在聊"。所有 CLI 实现细节（`cwd`、CLI session id、`--resume` 行为）对设备**完全透明**，
由 adapter（local_home 模式）或 cloud 控制台（cloud_saas 模式）代为管理。

→ 见 [ADR-014](decisions/ADR-014-logical-session-abstraction.md) 了解 session 抽象的具体落地方式。

## 文档结构

```
design/
├── README.md              # 本文件，文档索引
├── STATE_MACHINE.md       # 状态机设计文档（核心）
├── agent_bus.md           # Agent 总线架构（adapter 多路复用各 AI CLI）
└── decisions/            # 重要设计决策记录 (ADRs)
    ├── README.md
    └── ADR-001-adapter-as-agent-bus.md
```

## 核心原则

1. **文档优先**: 所有设计决策先文档再代码，代码实现必须与设计文档一致
2. **冲突解决**: 代码与文档不一致时，以文档为准，先解决设计问题再实现代码
3. **变更追踪**: 重大设计变更需在 `decisions/` 下记录决策过程

## 文档清单

| 文档 | 说明 |
|------|------|
| `STATE_MACHINE.md` | PTT状态机、App状态机、UI状态机等 |
| `agent_bus.md` | adapter 作为 Agent 总线，多 AI CLI 接入架构 |
| `firmware_agent_integration.md` | 固件侧接入 Agent Bus 的模块/UI/分阶段计划（Phase 4 蓝图）|

---

**重要性**: 本目录下的文档是开发决策的唯一真相来源。所有代码实现必须符合设计文档，如有冲突需先解决设计问题。

