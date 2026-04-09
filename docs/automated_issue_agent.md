# OpenClaw 自动化 Issue 执行策略

本仓库采用 **由维护者控制的标签工作流**，与 OpenClaw 等自动化代理对接：仅在被明确标记为可执行时，才由代理从 Issue 拉取任务并开 PR。

## 触发标签

- **`ready-for-agent`** — 唯一表示「允许自动化代理处理该 Issue」的标签。

## 策略

- 任何用户都可以开 Issue。
- **仅创建 Issue 不会**触发自动化执行。
- **只有维护者**可以为 Issue 添加 `ready-for-agent`。
- OpenClaw（或同等工具）**只处理**同时满足以下条件的 Issue：
  - 状态为 **open**；
  - 带有标签 **`ready-for-agent`**。

## 执行命令（维护者 / 运行环境）

在已配置 OpenClaw 与 GitHub 凭据的环境中，由维护者或 CI/cron 按需执行：

```bash
/gh-issues daboluocc/bbclaw --label ready-for-agent --limit 1 --cron
```

说明：`--limit 1` 控制单次处理数量；`--cron` 按各工具约定表示定时/批处理模式（以实际 OpenClaw 文档为准）。

## 工作流

1. 用户打开 Issue，描述问题或需求。
2. 维护者审阅 Issue（范围、风险、是否适合代理）。
3. 若任务**清晰、有边界、且适合自主实现**，维护者添加 **`ready-for-agent`**。
4. OpenClaw 拉取该 Issue，创建修复分支、实现改动并打开 **PR**。
5. 维护者**人工**测试、Review 并合并 PR；合并后可按需移除或调整标签（见下文配套标签）。

## 非目标（不应交给代理自动处理）

以下类型 Issue **不要**打 `ready-for-agent`，应由人工主导：

- 安全相关（漏洞、凭据、供应链等）
- 基础设施 / 运维面变更
- 大规模重构
- 边界模糊的大型功能设想
- 明确会破坏兼容性的变更（breaking changes）

若不确定，优先使用 **`needs-human-decision`** 或 **`blocked`**，不要加 `ready-for-agent`。

## 建议配套标签

可在本仓库中创建并使用下列标签，便于状态追踪（名称可按团队习惯微调，但 **`ready-for-agent` 语义应与本页一致**）：

| 标签 | 用途建议 |
|------|----------|
| `ready-for-agent` | 维护者确认：可交给自动化代理执行 |
| `agent-running` | 代理已认领 / 执行中 |
| `agent-pr-opened` | 代理已提交 PR，待人审 |
| `needs-human-decision` | 需产品或架构决策，不适合自动执行 |
| `blocked` | 依赖外部条件，暂停 |
| `bug` | 缺陷类 |
| `enhancement` | 改进/功能类 |
| `docs` | 文档类 |
| `refactor` | 重构类（通常不自动执行，除非范围极小且维护者明确） |

## 相关链接

- [参与共建 / 贡献流程](../CONTRIBUTING.md)
- [AGENTS.md（AI 与自动化指引）](../AGENTS.md)
