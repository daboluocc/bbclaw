# ADR-022: 记忆整理(consolidation) —— 收件箱 + 画像文档两层记忆

- **日期**: 2026-06-02
- **状态**: 已接受（v1 范围明确;5 项决策已定稿,供子任务 A/B/C 直接实现）
- **关联**: ADR-021（对话编排管家 §4 —— 本 ADR 是其续作,在「收件箱」之上叠加「画像文档」整理层）、ADR-020（记忆管线 —— 抗投毒三道防线 / 落盘安全 / ≤4KB clamp 思路沿用）、ADR-018（设备管家 §2 记忆设计）、ADR-014（逻辑会话）、`docs/PROJECT_PROFILE.md`

## 背景

ADR-021 §4 已落地（#83）「管家长期记忆」的**第一段**:每个管家 turn 末经 `Deps.MemoryWriter.RecordTurn` 把本轮蒸馏成 JSON delta,**append** 进 workspace `CLAUDE.md` 的 `<!-- BEGIN/END BBClaw-managed -->` 托管段(hash 去重 + ≤4KB FIFO clamp + 原子写 0600)。这一段是**流水账式**的:每轮一条要点、单调累积、到 4KB 就 FIFO 丢最旧。

问题:**流水账不是画像**。随着对话增多:

1. 同一偏好被反复记录(「我喜欢简短回复」可能记 5 遍),浪费 4KB 配额。
2. 新决策覆盖旧决策时(「项目换到 /proj/B 了」),旧条目仍占位,FIFO 可能先把有用的旧偏好挤掉而把过期决策留着。
3. 一坨混在一起的 bullets,管家 `--resume` 加载时无结构,难以"按需只看偏好"。

ADR-021 §4 自己也点出这个张力:**「仅移到归档尾段不强删」与「≤4KB 硬上限」可能矛盾** —— 不整理,记忆要么膨胀要么乱序丢失。

**本 ADR 补齐第二段:后台 consolidation(整理)。** 把流水账式的「收件箱」定期归类、去重、收敛成结构化的「画像文档」,清空收件箱腾出配额。这就是用户说的「让管家越聊越懂我」从"记流水账"升级成"沉淀画像"。

## 决策

**两层记忆模型**:收件箱(inbox,现状不动)+ 画像文档(profile docs,本 ADR 新增),由后台 consolidation 把前者归档进后者。

```
  ┌─────────────────────────── 管家 turn (ADR-021 §4, #83, 现状不动) ───────────────────────────┐
  │  每 turn 末 RecordTurn → 蒸馏 JSON delta → append 进 CLAUDE.md 托管段「收件箱」              │
  └──────────────────────────────────────────┬──────────────────────────────────────────────────┘
                                              │ 触发器(阈值/空闲/兜底 + per-key cooldown)
                                              ▼
  ┌──────────────────────── 后台 consolidation (本 ADR 新增第二段后台流程) ─────────────────────┐
  │  Option X: adapter 编排 ── 读收件箱全文 → `claude -p`(Haiku) 仅产「归类 JSON」              │
  │  → adapter 按 JSON 把条目分发/去重/收敛进画像文档 → 原子写 → 清空收件箱                      │
  └──────────────────────────────────────────┬──────────────────────────────────────────────────┘
                                              ▼
  ┌──────────────────────────────── workspace/MEMORY/ 画像文档 ─────────────────────────────────┐
  │   preferences.md   ·   projects.md   ·   decisions.md      (v1 三维度;people/glossary = 未来) │
  └─────────────────────────────────────────────────────────────────────────────────────────────┘
                                              ▲ CLAUDE.md「长期记忆(按需读取)」索引段软引导
                                              │ 管家自行按需 Read(不强制会话开始预读)
```

### 0. 两层职责边界(核心)

| 层 | 落点 | 写入方 | 形态 | 生命周期 |
|---|---|---|---|---|
| **收件箱 (inbox)** | workspace `CLAUDE.md` 托管段(现状,ADR-021 §4) | 每 turn `RecordTurn`(ADR-021 §4,**不动**) | 流水账 bullets,append-only | 被 consolidation 消费后**清空** |
| **画像文档 (profile docs)** | `workspace/MEMORY/{preferences,projects,decisions}.md` | 后台 consolidation(本 ADR) | 结构化、去重、按维度归类 | 长期保留,按维度各自 clamp |

**关键不冲突点(对接 ADR-021 §4)**:consolidation 是**新增的第二段后台流程**,与 ADR-021 §4 的 `RecordTurn`(第一段)**并存不替换**。第一段照旧每 turn append 进收件箱;第二段周期性地把收件箱"搬运+归类"进画像文档并清空收件箱。读者**不应**误以为 consolidation 取代了 `RecordTurn` —— 二者是生产者(per-turn 蒸馏)与归档者(后台整理)的关系。

---

## 5 项决策定稿

> 以下逐条给出**最终唯一选择 + 理由**,供子任务 A/B 直接实现,不再是待定。

### 决策 1 — v1 维度文件集合:**preferences / projects / decisions 三个**

- **最终选择**:v1 只做三个维度文件,落 `workspace/MEMORY/`:
  - `preferences.md` —— 用户长期偏好(回复风格、语言、领域口味、工具偏好)。
  - `projects.md` —— 最近/常用工程项目(项目名 + 用途 + 状态;**不写绝对路径**,见落盘安全)。
  - `decisions.md` —— 关键决策与约定(「以后部署都走灰度」类用户级约定,**非**对助手行为的指令)。
- **理由**:这三维度直接对应 ADR-021 §4 蒸馏出的三类(长期偏好 / 最近项目 / 关键决策),归类 JSON schema 天然一一映射,零额外抽象。`people`(人物)/`glossary`(术语表)**列为 future**:v1 数据量不足以支撑、且易与 decisions/preferences 边界重叠,先不引入。
- **未来(非 v1)**:`people.md` / `glossary.md` 在画像规模变大、维度边界清晰后再加;新增维度只需扩 `MEMORY/` 下文件 + 归类 JSON 的 `category` 枚举,无需改架构。

### 决策 2 — 触发阈值:**收件箱≥75% / 空闲 10min / 兜底 24h + per-key cooldown**(确认默认)

- **最终选择**:三个触发条件 **OR** 关系,任一满足即排队一次 consolidation;再叠加 **per-device(per-key)cooldown** 防抖:
  - **容量触发**:收件箱占用 **≥75%**。**基准 = ADR-021 §4 的收件箱 ≤4KB 硬上限**(即 ≥3KB / 4KB)。⚠️ 明确口径:75% 是相对**收件箱自身的 4KB 上限**,**不是**画像文档的配额;子任务实现阈值判断时以收件箱托管段当前字节数 / 4096 计算,避免口径不一。
  - **空闲触发**:管家会话**空闲 ≥10min**(距上一个 turn 结束)且收件箱非空 → 趁设备不忙整理。
  - **兜底触发**:距上次成功 consolidation **≥24h** 且收件箱非空 → 保证低频用户也定期收敛。
  - **per-key cooldown**:同一 `deviceID`(管家维度)两次 consolidation 间隔 **≥ cooldown(默认 10min)**,即使容量反复越过 75% 也不抖动重整。
- **理由**:容量触发防膨胀(逼近 4KB 前先收敛),空闲触发抓"低打扰窗口",兜底触发覆盖长尾,cooldown 防止短时间内反复触发空转 LLM。三值与 cooldown 全部走 env 可覆盖(`BBCLAW_BUTLER_MEMORY_CONSOLIDATE_*`),默认即上述。
- **触发器状态机**(子任务实现参照):

  ```
        ┌──────┐  收件箱≥75% / 空闲≥10min / 距上次≥24h(且收件箱非空)
        │ IDLE │ ───────────────────────────────────────────────►┐
        └──────┘                                                   │
           ▲                                                       ▼
           │                                            ┌──────────────────┐
           │  cooldown(默认10min)未到 → 丢弃本次触发,回 IDLE  │  COOLDOWN check  │
           │                                            └────────┬─────────┘
           │                                          cooldown 已过 │
           │                                                       ▼
           │                                            ┌──────────────────┐
           │   失败:全吞(log+metric),收件箱保持不动,回 IDLE 等下次触发      │   RUNNING        │
           └────────────────────────────────────────────│  (单 worker,并发=1)│
              成功:写画像 → 清空收件箱 → 记 lastConsolidateAt → 回 IDLE      └──────────────────┘
  ```
  - 并发=1:与 ADR-021 §4 蒸馏 worker / WarmPool replenish **共享并发信号量**,绝不抢资源。
  - RUNNING 期间到达的新 turn 照旧 append 进收件箱(收件箱不锁写);consolidation 读的是**触发瞬间的快照**,清空时只清"已消费的那部分"(按 hash/offset),避免清掉整理期间新写入的条目(防丢)。

### 决策 3 — 整理编排:**Option X(adapter 编排,`claude -p` Haiku 仅做归类 JSON)**

- **最终选择**:**Option X**。consolidation 的控制流在 **adapter**(Go),LLM 只承担**纯归类**这一步:
  1. adapter 读收件箱托管段全文(快照)。
  2. adapter 起后台单 worker `claude -p`(Haiku,最便宜),prompt = 收件箱条目 + 现有画像文档摘要 + **严格归类指令**,要求**只输出归类 JSON**(见下「整理 prompt 规则」),不让 LLM 直接读写文件。
  3. adapter 解析 JSON,**按 Go 代码**把每条 delta 分发到对应维度文件、执行去重/收敛/clamp、原子写、清空收件箱。
- **明确排除 Option Y(agentic 自读写)**:不让 claude 在 workspace 里自主 Read/Write `MEMORY/*.md`。理由:
  - **安全可控**:落盘由 adapter Go 代码做,统一过 IsPoisoned/deny 过滤/0600/无绝对路径/原子写关卡(见落盘安全),不依赖 LLM 自觉。
  - **确定性**:归类是结构化任务,JSON schema 约束下 Haiku 足够;agentic 自读写引入多步工具调用、不确定的文件操作、更高延迟与成本。
  - **与 ADR-021 §4 一致**:第一段蒸馏已是"adapter 编排 + LLM 只产 JSON delta",第二段沿用同一范式,代码与心智模型统一。
- **归类 JSON schema**(v1):

  ```json
  {
    "items": [
      {
        "category": "preferences | projects | decisions",
        "text": "<收敛后的单条要点,陈述句,无指令>",
        "supersedes": "<可选:本条收敛/覆盖的旧要点摘要,用于决策4的归档语义>"
      }
    ]
  }
  ```
  - LLM 负责:把收件箱里语义重复的多条**合并成一条**、判定每条属于哪个 `category`、对"新覆盖旧"标 `supersedes`。
  - LLM **不负责**:删文件、写绝对路径、输出指令性内容(由 adapter 侧 deny 过滤兜底)。

### 决策 4 — 「新覆盖旧」:**v1 仅移到归档尾段,不强删**

- **最终选择**:当归类 JSON 标了 `supersedes`(新条目覆盖旧条目),v1 **不物理删除**旧要点,而是把旧要点**移到对应维度文件的 `<!-- archive -->` 归档尾段**(从活跃区移出,标记为被取代)。活跃区只保留最新有效条目。
- **配套回收(解决 ADR-021 §4 点出的「不删 vs ≤4KB 上限」矛盾)**:画像文档**不是无限增长**。每个维度文件设**独立配额 ≤4KB(沿用 ADR-020 思路)**,活跃区 + 归档尾段合计超限时:
  - **先 clamp 归档尾段**(FIFO 丢最旧的已归档条目)—— 被取代的旧记忆本就是回收首选,真正的"软删"发生在这里。
  - 归档尾段清空后仍超限,才 clamp 活跃区最旧条目。
  - 即:**v1 不在"覆盖"时强删,而在"配额超限"时按 FIFO 回收**,归档区优先。这样「不强删」(语义安全:覆盖不等于立即丢)与「≤4KB 硬上限」(膨胀可控)**两者不再矛盾**。
- **理由**:v1 保守 —— 误判"覆盖"时旧记忆还能从归档区找回(consolidation 是 LLM 启发式,可能错判);同时配额 + 归档优先 FIFO 保证文件不膨胀。**真删(物理移除被取代条目)列为 future**,待 consolidation 判定足够可信再开。

### 决策 5 — CLAUDE.md 加载:**仅「按需读」软引导**

- **最终选择**:workspace `CLAUDE.md` 增加一段**「长期记忆(按需读取)」索引段**(同样在 BBClaw-managed marker 内),内容是对画像文档的**软引导**:列出 `MEMORY/preferences.md`、`MEMORY/projects.md`、`MEMORY/decisions.md` 的路径与一句话用途,提示管家**在需要时**自行 `Read` 对应文件。
- **明确排除「会话开始先读 preferences」强约定**:v1 **不**在每次会话开始强制预读任何画像文件。
- **理由**:
  - **省 token / 省延迟**:管家每轮都全量注入三份画像文档会线性涨 prompt 成本(ADR-020 已接受 system prompt 注入成本,但画像可能比收件箱大得多);软引导让 claude 仅在相关 turn 才按需读。
  - **claude 原生能力**:管家是完整 claude 会话,有文件读取工具(cwd=workspace),"看到索引→按需 Read"是它擅长的;不必 adapter 越俎代庖把内容塞进 system prompt。
  - **与现状一致**:收件箱仍随 `CLAUDE.md` 被 cwd 隐式加载(ADR-021 闸门②),画像文档作为"可按需展开的附录"由索引段指路,职责清晰。
- **future**:若实测发现管家"该读不读"导致遗忘偏好,再升级为"会话开始预读 preferences.md(仅最小那份)"的强约定;v1 先软引导。

---

## 整理 prompt 规则(Option X 的 LLM 步)

- **输入**:收件箱托管段全文 + 三份画像文档的**当前活跃区摘要**(让 LLM 知道"已经记过什么",避免重复归类)。
- **输出**:**仅** 上述归类 JSON,无散文、无 markdown 围栏外内容。
- **归类约束**(prompt 内显式写明):
  - 只归纳「领域偏好 / 项目事实 / 用户级决策约定」三类;**丢弃任何对助手行为/权限/约束的指令**(沿用 ADR-020 §2 蒸馏 prompt 约束)。
  - 语义重复的多条合并成一条;能判定"新取代旧"的标 `supersedes`。
  - 不输出绝对路径、密钥、对话原文长引用 —— 只留可持久的事实陈述。
- **deny 过滤(adapter 侧,LLM 之后兜底)**:解析 JSON 后,对每条 `text` 再过一遍 ADR-020 §2 的 deny 规则(含 `permission`/`bypass`/`ignore previous`/`system prompt`/`你现在是…` 等指令式内容整条丢),**不信任 LLM 自觉**。这是抗投毒第二道防线在第二段流程的延续。

## 落盘安全(沿用 ADR-020 / ADR-021 §4)

- **IsPoisoned**:写画像前对收件箱快照与归类结果做投毒检测(指令性话术、超长异常条目),命中则整批丢弃 + log,不写盘。
- **0600**:`MEMORY/*.md` 与其所在目录均 `0600`/`0700`(含用户事实,不用 0644)。
- **无绝对路径**:`projects.md` 只写项目名/用途,**不写 cwd 绝对路径**(隐私);归类 JSON 若混入路径,adapter 侧剥离。
- **原子写**:每个维度文件走 `tmp + fsync + rename`(复用 `logicalsession`/`workspace` 既有原子写骨架);多文件更新各自原子,单文件失败不污染其他维度。
- **marker 隔离**:画像文档若与用户手写共存,同样用 `<!-- BEGIN/END BBClaw-managed -->` marker 段隔离 + hash 幂等(同 ADR-020 §3 / ADR-021)。
- **清空收件箱的安全**:consolidation 成功落盘后才清收件箱,且只清"触发快照内已消费的条目"(按 hash/offset),整理期间新 append 的条目保留,防丢。

## 范围限定:仅 LOCAL

- v1 consolidation **只在 LOCAL 范围**触发与写入,**cloud 多租户 v1 不写**(与 ADR-020 §4 / ADR-021 §4 一致:user 维度落地前避免跨租户串写/污染)。
- env `BBCLAW_BUTLER_MEMORY_CONSOLIDATE`(默认 **off**),链路 smoke 打通后 LOCAL 灰度开;cloud 路径整步跳过。

## 影响

### 正面
- 记忆从"流水账"升级成"结构化画像":同类偏好去重收敛、决策有覆盖语义、按维度可分别按需读 —— 管家"越聊越懂你"真正成立。
- 解决 ADR-021 §4 自暴的「不删 vs ≤4KB」矛盾:覆盖走归档、配额按 FIFO 优先回收归档区,膨胀可控且不立即丢。
- 复用既有范式:Option X 沿用"adapter 编排 + LLM 只产 JSON"(同 ADR-021 §4 第一段),抗投毒/落盘安全全部复用 ADR-020/021 关卡,新增面集中在"触发器 + 归类分发 + MEMORY/ 文件管理"。
- 软引导加载省 token/延迟,不强制每轮注入画像。

### 负面 / Tradeoff
- 多一段后台 LLM 流程(Haiku 归类),低频但增成本/复杂度(默认 off + cooldown + 仅 LOCAL 缓解)。
- 画像文档是新增的磁盘状态(三份 MEMORY/*.md),与收件箱、各项目 CLAUDE.md 画像并存,记忆轴变多(职责边界表已厘清)。
- "新覆盖旧"靠 LLM 启发式判 `supersedes`,可能误判(v1 不强删 + 归档可找回缓解)。
- 软引导依赖 claude "该读就读",可能漏读(future 可升级强约定)。

### 中性
- ADR-021 §4 的 per-turn `RecordTurn` 收件箱写入**不变**;ADR-020 §3 各项目 cwd 的 CLAUDE.md 项目画像仍是独立轴,不受影响。

## 风险与缓解

| 严重度 | 风险 | 缓解 |
|---|---|---|
| high | 记忆投毒经画像文档持久化、每次按需读放大 | IsPoisoned + adapter 侧 deny 过滤(ADR-020 §2 延续)+ 归类 prompt 丢弃指令性内容,三道防线沿用到第二段 |
| medium | consolidation 误判"覆盖"丢掉仍有用的旧记忆 | v1 **不强删**,仅移归档尾段,可找回;真删列 future |
| medium | 整理期间新 turn append 被清空误删 | 只清触发快照内已消费条目(hash/offset),整理期新写入保留 |
| medium | 75% 基准口径不一致(子任务实现阈值跑偏) | 明确 = 收件箱自身 4KB 上限的 75%,非画像配额;单一 env + 单处计算 |
| medium | 画像文档无限膨胀(与"不删"冲突) | 每维度 ≤4KB 独立配额 + 归档区优先 FIFO 回收 |
| low | Haiku 归类 cold-spawn 占资源 / 抢 WarmPool | 单 worker 并发=1 共享信号量 + cooldown 限频 + 默认 off |
| low | 画像文档绝对路径/密钥泄漏 | projects.md 不写绝对路径;0600;deny + 路径剥离 |

## 备选方案(已排除)

1. **Option Y(agentic 自读写画像)**:让 claude 自主 Read/Write `MEMORY/*.md` —— 落盘失控、不确定、延迟/成本高;v1 选 Option X(见决策 3)。
2. **覆盖即强删旧记忆**:LLM 启发式判 `supersedes` 可能误判,强删不可逆;v1 移归档 + FIFO 回收(见决策 4)。
3. **会话开始强制预读全部画像**:每轮 token 线性涨;v1 软引导按需读(见决策 5)。
4. **不分维度、单一 profile.md**:无法"只读偏好"、去重/clamp 粒度粗;v1 分三维度文件。
5. **加 people/glossary 进 v1**:数据量不足、边界与现有维度重叠;列 future(见决策 1)。
6. **每 turn 即整理(无收件箱)**:成本/延迟不可接受,且失去"批量去重收敛"价值;保留收件箱 + 周期 consolidation(见决策 2)。

## 实现 checklist

**前置(已由 ADR-021 §4 / #83 提供)**
- [x] 收件箱:per-turn `RecordTurn` → CLAUDE.md 托管段 append(hash + ≤4KB FIFO + 0600 原子写)
- [x] 落盘骨架:`workspace.ReplaceManagedBlock` / marker / hash 幂等 / 原子写

**v1(consolidation,默认 off,仅 LOCAL,灰度)**
- [ ] `workspace/MEMORY/` 脚手架:`preferences.md` / `projects.md` / `decisions.md`(各 marker 段 + ≤4KB 配额 + 归档尾段)
- [ ] 触发器状态机:容量(收件箱 ≥75% of 4KB)/ 空闲(≥10min)/ 兜底(≥24h)+ per-device cooldown(≥10min);env `BBCLAW_BUTLER_MEMORY_CONSOLIDATE_*` 可覆盖
- [ ] Option X 编排:读收件箱快照 → 单 worker(并发=1,共享信号量)Haiku `claude -p` 产归类 JSON → adapter 解析分发 → 去重/覆盖归档/clamp → 原子写 → 清空已消费收件箱
- [ ] 归类 JSON schema(category/text/supersedes)+ 整理 prompt(仅产 JSON、丢指令性内容)
- [ ] 安全:IsPoisoned + deny 过滤(ADR-020 §2 延续)+ 无绝对路径剥离 + 0600 + 各维度独立原子写
- [ ] CLAUDE.md「长期记忆(按需读取)」索引段(软引导,marker 内;不强制预读)
- [ ] 范围闸:LOCAL 触发/写入;cloud 整步跳过;env 默认 off
- [ ] 测试:触发器(三条件 + cooldown 防抖)、归类分发(三维度落位)、覆盖→归档语义、配额 clamp(归档优先 FIFO)、deny/IsPoisoned、清空只清已消费快照、cloud 跳过、env 默认 off

**future(非 v1)**
- [ ] 维度扩展:`people.md` / `glossary.md`(扩 category 枚举)
- [ ] 覆盖即真删(consolidation 判定够可信后)
- [ ] CLAUDE.md 会话开始预读 preferences(软引导漏读时升级)
- [ ] cloud 多租户 user 维度 consolidation

## 需用户拍板(决策已在上文定稿,此处仅列回溯锚点)

> 本 ADR 状态为「已接受」,以下 5 项均已给出**最终选择**(见「5 项决策定稿」),保留此节供子任务回溯;如需调整,先改本 ADR 再改代码(CLAUDE.md「设计先行」原则)。

1. v1 维度文件 = **preferences / projects / decisions 三个**(people/glossary = future)。
2. 触发阈值 = **收件箱≥75%(of 4KB)/ 空闲 10min / 兜底 24h + per-key cooldown 10min**。
3. 整理编排 = **Option X**(adapter 编排,`claude -p` Haiku 仅产归类 JSON)。
4. 「新覆盖旧」= **v1 仅移归档尾段不强删**,配额超限时归档区优先 FIFO 回收。
5. CLAUDE.md 加载 = **仅「按需读」软引导**,不强制会话开始预读。
