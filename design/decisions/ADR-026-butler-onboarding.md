# ADR-026: 管家初次见面(onboarding) —— 确定性注入而非 persona 软指引

- **日期**: 2026-06-11
- **状态**: 已接受
- **关联**: ADR-021（对话编排管家 —— workspace persona / CLAUDE.md 原生加载）、ADR-022（画像文档 —— `MEMORY/profile.md` 属于其维度文件集）、ADR-018 §3（系统提示经 `--append-system-prompt` 每轮注入）

## 背景

workspace persona（`internal/workspace/persona.go` 的 `DefaultClaudeMD`）已包含「初次见面（初始化）」一节：`MEMORY/profile.md` 带 `<!-- STATUS: uninitialized | initialized | skipped -->` 标记，管家应在 STATUS 为 `uninitialized` 时顺势自我介绍、询问称呼/角色，把答案写入 profile.md 并翻转标记。

实测（2026-06-11，全新管家会话）：**模型并不可靠执行这段软指引**。用户说「在吗」，管家只回了一句招呼，既没自我介绍也没发起初始化——CLAUDE.md 里的 persona 指引淹没在长文档中，模型对"顺势发起"的判断过于保守。结果是 profile.md 永远停在 `uninitialized`，初始化流程形同虚设。

教训与 ADR-021 §4 记忆管线一致：**凡是"必须发生"的行为，要由 adapter 代码确定性驱动，不能指望模型自觉。**

## 决策

**当（且仅当）本轮是管家会话且 profile 未初始化时，由 adapter 在每轮系统提示中注入一段「初次见面」硬指令；标记翻转后注入自动消失。**

1. **状态读取**：`workspace.ProfileStatus()` 读 `MEMORY/profile.md` 的 `<!-- STATUS: xxx -->` 标记，返回 `uninitialized` / `initialized` / `skipped`；文件或标记缺失/不可读返回 `""`（视为"不注入"，损坏的 profile 绝不能造成反复骚扰用户）。
2. **注入点**：`butler.DeviceSystemPrompt`（ADR-018 §3，每轮经 `StartOpts.SystemPrompt` → `--append-system-prompt` 下达）。该函数是 LOCAL 与 cloud-relay 两条路径共用的注入函数，一处改动全路径生效。
3. **门控**：`cwd == workspace.Dir()`（即管家会话）**且** `ProfileStatus() == uninitialized`。worker 会话（项目目录 cwd）永不注入——worker 不许自我介绍、不许盘问用户。
4. **指令内容**：先回应用户当下请求 → 一两句自我介绍（BBClaw 管家、能聊天/查项目/派发编码任务）→ 自然询问怎么称呼（角色/职业之后几轮慢慢补）→ 答案立即写入 profile.md 并把 STATUS 改 `initialized`（拒绝则 `skipped`）→ 紧急请求时本轮可跳过。
5. **终止条件**：管家把 STATUS 写成 `initialized` 或 `skipped` 后，下一轮 `ProfileStatus()` 不再返回 `uninitialized`，注入自动停止。无需额外状态、无需冷却——profile.md 本身就是状态机。

## 备选方案（已否决）

1. **只加强 CLAUDE.md persona 措辞**：仍是软指引，实测不可靠；且 CLAUDE.md 是用户可改的文件，无法保证存在（ADR-021 scaffold 对已存在文件不升级）。
2. **adapter 代发固定开场白**：绕过模型直接 TTS 一段介绍。否决——开场白无法衔接用户当下的请求，体验生硬；且写 profile.md 仍需模型执行。
3. **每轮无条件注入**：浪费 token，且初始化完成后继续注入会诱导模型重复发起。门控在 STATUS 上成本最低。

## 影响

- `internal/workspace/profile.go`（新增）：`ProfileStatus` / `ParseProfileStatus` + STATUS 常量。
- `internal/butler/persona.go`：`DeviceSystemPrompt` 末尾追加 `onboardingSection(cwd)`。
- persona（`DefaultClaudeMD`）的「初次见面」一节保留——它仍是 STATUS 语义和写入路径的文档；本 ADR 只是把**触发**从软指引升级为硬注入。
