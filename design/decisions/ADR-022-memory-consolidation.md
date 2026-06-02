# ADR-022: 记忆沉淀引擎 —— 收件箱归档进 MEMORY 多维画像并清空

- **日期**: 2026-06-02
- **状态**: Accepted（v1，LOCAL-only，默认关闭灰度）
- **关联**: ADR-021 §4（管家长期记忆唯一落点 = workspace `CLAUDE.md` 托管段 / 蒸馏管线）、ADR-020 §2（抗投毒三道防线 / deny 过滤）、ADR-018（设备管家记忆设计）。本 ADR 是 ADR-021 §4 「per-turn distill（收件箱 append）」之上的**第二层**。
- **实现 issue**: #92（父任务 #89 · 子B）。目录/路径依赖 #91（本 ADR 采用**防御式自建** `MEMORY/` 路径以解耦，见 §3）。

## 背景

ADR-021 §4 已落地「per-turn distill」：每轮管家对话蒸馏成几条 note，append 进 workspace `CLAUDE.md` 的 `<!-- BEGIN/END BBClaw-managed -->` 托管段（「收件箱」）。该段受 `DefaultMaxBytes=4096` 硬上限约束，超限时 **FIFO 静默丢弃最旧条目**（`store.go mergeManaged`）。

问题：4KB FIFO 是「静默丢失硬上限」——一旦写满，旧记忆被无声驱逐，没有任何「整理 / 沉淀」机会。用户偏好、长期项目这类高价值事实会和一次性细节一起被冲走。

## 决策

在 per-turn distill 之上新增**第二层「沉淀（consolidation）」**：后台把收件箱归档进 `MEMORY/*.md` 多维画像并清空收件箱，使 4KB 从「FIFO 静默丢失硬上限」降级为「整理缓冲」。

复用 ADR-021 §4 的全部底层原语：单 worker（并发=1）、`IsPoisoned` deny 过滤、`atomicWrite(0600)`、`claude -p` Haiku 范式、`workspace.ManagedBlock/ReplaceManagedBlock`。

### 1. 触发器（四类 + cooldown）

挂在 `MemoryWriter.RecordTurn`（engine.go:538，仅 `RoleButler && turnEnded && errorCount==0`）打 turn 末时间戳。`RecordTurn` 契约要求**严格非阻塞**，时间戳更新只做一次轻量赋值（mutex 保护）。

触发判定是一个纯函数 `decideTrigger(state, cfg)`，四类触发：

| 触发 | 条件 | 默认值（env 可覆盖） |
|---|---|---|
| **阈值** | 收件箱字节 ≥ `thresholdRatio × maxBytes` | `thresholdRatio=0.75`（即 ≥75%，issue 第 1 项） |
| **空闲** | `now - lastTurnAt ≥ idleGap` 且收件箱非空 | `idleGap=5m` |
| **兜底** | `now - lastConsolidateAt ≥ maxGap` 且收件箱非空 | `maxGap=6h` |
| **cooldown** | `now - lastConsolidateAt < cooldown` → 一律不触发 | `cooldown=10m` |

- 阈值在 worker 每次处理完一个 turn 后检查（同 worker，串行）。
- 空闲 / 兜底由一个轻量 ticker goroutine 周期性向 worker 投递「检查」信号；真正的决策仍在 worker 上经 `decideTrigger` 做，I/O 与决策分离。
- 收件箱为空时所有触发都返回 false（无可沉淀）。

> **为何这些默认值**：issue 把触发器默认值 defer 给本 ADR（第 2 项）。本 v1 取「阈值 75% / 空闲 5min / 兜底 6h / cooldown 10min」作为保守默认——既保证写满前有整理机会（75%），又避免设备活跃时频繁打断（cooldown + idle），兜底确保即使长期低于阈值也会定期沉淀。全部 env 可调，灰度中按真实数据回调。

### 2. 整理引擎（Option X）

worker 上串行执行 `Consolidate(ctx)`：

1. 读 `CLAUDE.md` → `ManagedBlock` 取收件箱 inner（**快照**）。空则直接返回 nil（幂等）。
2. 读现有 `MEMORY/*.md` 各维度画像。
3. `claude -p` Haiku：输入「收件箱 bullets + 现有各维度画像」，输出 JSON 对象 `{preference:[...], project:[...], decision:[...]}`。
4. 经 `IsPoisoned` **双过滤**（prompt 约束 + 落盘前过滤）。
5. 每维度 `maxPerDim` 上限裁剪。
6. **0600 原子写** 各 `MEMORY/<dim>.md`（无绝对路径）。
7. **仅当全部写盘成功**才清空收件箱（见 §4 时序）；任一步失败全吞（log），**绝不清空**。

**spike 结论（issue 实现期硬前置）**：`claude -p` Haiku 对数十条 bullets 输出大 JSON 的稳定性，本实现采用与 `distiller_claude.go parseItems` 同源的**容错切片**（从首个 `{` 到末个 `}`），而**非** `--output-format json`——后者会把输出包进 stream/result 信封，反而增加解析层级。容错切片 + 每维度上限 + `IsPoisoned` 双过滤共同把「脏输出污染画像」的爆炸半径限制住：解析失败 → 返回 error → 不清空收件箱（下轮重试）；解析出脏条目 → 被 deny 过滤拦截。**本仓 CI / 本任务环境无 claude CLI + API key，未跑真链路**；解析与过滤是防御式设计，真链路冒烟留集成测试（`BBCLAW_BUTLER_LIVE=1`）。

### 3. 整理 prompt 规则

- **合并 / 去重**：把收件箱新条目并入现有维度画像，语义去重。
- **新覆盖旧（真删）**：当新事实取代旧事实时，**物理删除旧条目**（不保留历史）。每次沉淀对 `MEMORY/<dim>.md` 做**整文件重写**（LLM 合并后的全量输出落盘），故被取代的事实不再出现 —— issue 第 4 项「真删与否」本 v1 取**真删（hard override）**，语义最简、画像最干净。
- **剔除过期 / 丢指令性内容**：过期事实丢弃；任何「对助手行为/权限/身份的指令」丢弃（与 ADR-020 §2 一致）。
- **每维度上限**：`maxPerDim`（默认 30 条）封顶，超出由 LLM 取最重要的保留，落盘再做一次硬裁剪兜底。

### 4. 约束与时序

- **单 worker 复用**：consolidation 任务与 per-turn distill 的 append 任务跑在**同一个并发=1 worker**上，天然串行化，互不并发；二者在 worker 队列排队，互不饿死（distill 走 `ch`，consolidation 走独立信号 + worker select，distill 不会被 consolidation 长时间 LLM 调用永久阻塞，反之亦然 —— ticker 满即丢，下次再触发）。
- **读-清空与 append 竞态**：清空采用**快照感知清除**——只移除「快照里出现过的行」，沉淀期间（LLM 调用耗时秒级）新 append 进收件箱的行**不在快照集合内，予以保留**。即使未来 consolidation 移出 worker 并发执行，此设计仍正确。AC 要求并有测试覆盖。
- **仅 LOCAL**：cloud 多租户 v1 不接线（per-user scoping 落地前避免串写），与 ADR-021 §4 一致。
- **路径解耦 #91**：`MEMORY/` 目录防御式自建为 `filepath.Dir(CLAUDE.md)/MEMORY`（即 `~/.bbclaw-adapter/workspace/MEMORY/`），不依赖 #91 的目录约定，避免互相 block。

### 5. env 门控

沿用 `BBCLAW_BUTLER_MEMORY_*` 命名约定：

| env | 默认 | 含义 |
|---|---|---|
| `BBCLAW_BUTLER_MEMORY_CONSOLIDATE` | off | 沉淀引擎总开关（默认关，灰度）。需 `BBCLAW_BUTLER_MEMORY_DISTILL` 也开（沉淀依赖收件箱有内容）。 |
| `BBCLAW_BUTLER_MEMORY_CONSOLIDATE_THRESHOLD` | 0.75 | 阈值比例 |
| `BBCLAW_BUTLER_MEMORY_CONSOLIDATE_IDLE` | 5m | 空闲间隔 |
| `BBCLAW_BUTLER_MEMORY_CONSOLIDATE_MAXGAP` | 6h | 兜底间隔 |
| `BBCLAW_BUTLER_MEMORY_CONSOLIDATE_COOLDOWN` | 10m | per-key cooldown |
| `BBCLAW_BUTLER_MEMORY_CONSOLIDATE_MAXPERDIM` | 30 | 每维度上限 |

模型 / 二进制复用 `BBCLAW_BUTLER_MEMORY_MODEL` / `BBCLAW_BUTLER_MEMORY_CLAUDE_BIN`。

## 后果

- 收件箱 4KB 从「静默丢失」升级为「整理缓冲」：写满前由沉淀引擎归档进无上限的 `MEMORY/*.md` 多维画像。
- 新增一个独立子系统（触发状态机 + 整理引擎 + 多文件画像布局），但全部复用现有原语，边界清晰、单 PR 可落地。
- v1 默认关闭、LOCAL-only、灰度推进；真链路稳定性留集成冒烟验证。
