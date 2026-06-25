# ADR-036: adapter_v2 项目载入与系统提示项目清单注入

- **日期**: 2026-06-25
- **状态**: 进行中（设计定稿。**P1 核心已实现**：`internal/projectstore` 包 + `/v1/projects` CRUD + 原生目录选择器 + `DeviceSystemPrompt` 渲染项目清单(boot 读盘) + `ASR_HOTWORDS` 合并项目名 + `cliReady` + 管理页项目卡片；`go test -race ./...` 全绿。**待补**：移植 v1 prewarm 的 scanner 写 `MEMORY/projects.md`(§决策三)。未真机验证。）
- **UX 修订（2026-06-25，owner 反馈）**：推翻原先两条决策——(1) **加/删项目不再强制重启**：配置项目是低优先级 setup，不需立刻生效，持久化即可、下次适配器**自然重启**时焊进系统提示（见 §决策二）；(2) **目录选择改用宿主机原生「打开本地目录」对话框**（osascript / zenity），不在浏览器里渲染目录树（见「HTTP/UX 契约」）。下文相关段落已就地更新并标注「修订」。
- **关联**:
  - **ADR-025（web-first 配置）**——直接复用其 env-overlay store 范式、loopback-only 管理页、`adminapi.Restart()` re-exec 的「restart-to-apply」心智。项目载入是同一配置心智的延伸，不另立配置体系。
  - **ADR-022（记忆整理 + 画像文档）**——项目「重知识」落 `MEMORY/projects.md`，属画像维度；scanner 写入须遵守其两层模型与反投毒（无绝对路径进画像、deny 过滤、0600）。
  - **ADR-020（记忆管线）**——反投毒三防线（deny + IsPoisoned + prompt 约束）+ 画像文档「不每轮强加载」的原始约束。本 ADR 正是用「项目清单进系统提示」绕过软指引不可靠（见 §决策二）。
  - **ADR-023 / ADR-024（driver 管理 / 多-driver 生态）**——「无 git 要求、目录即项目、CLI 须装」的依据；据此**否决 driver-plugin 方案**（v2 P1 无 driver 层）。
  - **ADR-021（对话式编排管家）**——butler workspace cwd 不可变、persona boot 构建的依据，解释为何走 re-exec 而非 per-turn / respawn。
  - **ADR-018（设备管家架构）**——v1 `list_projects`/`dispatch` MCP 的来源；本 ADR **暂不移植 dispatch**（见 §分期）。
  - **ADR-026（管家 onboarding）**——`profile.md` STATUS gate；项目载入**不走** onboarding 注入、不碰 `profile.md` STATUS。
  - v1 参考实现：`adapter/internal/projectstore/store.go`、`adapter/internal/prewarm/scan.go`、`adapter/internal/httpapi/admin.go`、`adapter/internal/butlermcp/server.go`。

## 背景

管家被语音问到某项目（截图：「你知道 Buildhub 吗？」）时答不上来：`bbclaw-buildhub` 没装、`BBCLAW_BUILDHUB_BIN` 为空，拉不到 issue 列表。复盘出**两个独立根因**：

1. **系统提示里没有项目清单**——管家完全不知道用户手上有哪些项目、各自干什么。
2. **专属 CLI 调不到**——`BBCLAW_BUILDHUB_BIN` 空、`bbclaw-buildhub` 不在 PATH，管家既不知道有这个工具也无从调用。

v1 adapter 本有一整套：`projectstore`（管理页登记目录）+ `prewarm`（扫技术栈/README/git commits 写 `MEMORY/projects.md`）+ `list_projects`/`dispatch` MCP 工具（`adapter/internal/butlermcp/server.go:222-273`）。adapter_v2 P1 **故意省了**这一层（`adapter_v2/internal/butler/persona.go` 顶部注释：v2 简化、claude 自管 MEMORY/、无 dispatch MCP）。诉求：把「项目载入」带回 v2，且对**非程序员友好**（加个任意目录就行，不限 git），并**保证不遗忘**（加入后更新进系统提示）。

## 现状约束（决定方案的硬事实，均经代码核对）

- **persona / 系统提示 boot 时构建一次、跨设备共享**：`DeviceSystemPrompt(cwd, deviceID)` 返回静态串，经 `--append-system-prompt` 注入（`adapter_v2/internal/butler/workspace.go:96`）；persona 在 main boot 时算好 `baseArgv` 传入 `DeviceSession`（`session.go:30` 注释「persona already baked in」）。
- **CLAUDE.md 是常量**，`EnsureWorkspace` 幂等写一次（`workspace.go:51-55`）——无法在 boot 时动态插值。
- **没有 per-turn SystemPrompt 钩子**：v1 有 `engine.go:51-54` 的 `Deps.SystemPrompt`，v2 走 PTY 不走 engine，没有这条注入路径。
- **`respawn()` 不重建系统提示**：`DeviceSession.respawn`（`session.go:206`）只 `mgr.Remove(DefaultID)` 换 resume 的会话 id，不重算 `baseArgv` ⇒ 拿不到新项目。
- **已有 re-exec**：`adminapi.Restart()` 用 `syscall.Exec` 原地替换进程（带 `os.Environ()`），页面 poll `/healthz` 重连（ADR-025）。
- **`ASR_HOTWORDS` 只做 ASR boost、不做意图路由**：`voicekit.go:115-151` 把它喂给 ASR 后端提升识别；意图完全由 butler LLM 读转写决定，无热词→意图映射层。
- **settingsstore 坏文件降级范式**：`settingsstore.Open` 对损坏文件返回可用空态、不阻塞 boot——项目 store 沿用。

## 决策

### 决策一：独立 `projectstore`（sibling `projects.json`），不塞进 Settings

移植 v1 `adapter/internal/projectstore` 到 `adapter_v2/internal/projectstore`，持久化为 **sibling 文件** `~/.bbclaw-adapter-v2/projects.json`（与 `settings.json`/`identity.json` 同 `settingsstore.DataDir()`）。**不**塞进 `Settings`：项目是 list 增删，与 settings 的「整文档 PUT」语义不合。

`Project` 结构沿用 v1（`store.go:47-53`）+ 两个 v2 新字段：

```
Project { Name, Path(绝对), Source('admin'|'env'), AddedAt,
          Summary(一句话用途,用户填或 scanner 推断),
          CLIBin(可选,该项目专属 CLI 的绝对路径或名字,如 bbclaw-buildhub) }
```

文件 `{version:1, projects:[...]}`，原子写（temp+rename, 0600）。校验沿用 v1 `Add`（`store.go:279-321`）：path 绝对 + 存在 + 是目录；name 非空且不含 `,`/`:`（留作 hotword/清单分隔安全）；name+path 池内唯一。**单一共享池**（决策已确认）：所有设备看到同一份 `projects.json`，不做 per-device 维度。

### 决策二：持久触达走 boot-once 读盘（下次自然重启生效，**不强制重启**）【修订 2026-06-25】

`DeviceSystemPrompt` 在构建时读 `projects.json`，把已登记项目渲染成**紧凑清单**追加到现有 walkie-talkie 约束之后，每条形如：

```
- <name> — <一句用途> — 目录 <path> — [CLI: <bin> 就绪 | 未配置]
```

加/删项目（`POST`/`DELETE /v1/projects`）**只持久化、不触发重启**。下次适配器**自然重启**（改设置 re-exec、部署、重开机等）时，`main.go` 重新 `EnsureWorkspace` + `DeviceClaudeArgs`，`DeviceSystemPrompt` 重新读盘，新项目就**焊进了每轮都带的系统提示**。

> **修订原因（owner 反馈）**：原设计加/删项目即复用 `adminapi.Restart()` re-exec 落地（~1-2s 断连）。owner 反馈「配置项目就行，不要重启，不用立刻生效」——配置项目是低优先级 setup，强制重启太重。改为**持久化即可、下次自然重启生效**：管理页加/删后只 toast「下次重启后生效」，不弹重启横幅、不调 `/v1/settings/restart`。
>
> **「不遗忘」仍成立**：项目清单已落盘 `projects.json`，下次 boot 必然读入系统提示——每轮强加载，不赌管家主动去读 `MEMORY/projects.md`（软指引）。与原设计唯一差别是**生效有延迟**（到下次重启），owner 已确认接受。
>
> **不要用 respawn**（仍写进 ADR 防后人踩坑）：`respawn` 只换会话 id、不重建 `baseArgv`/系统提示（`session.go:30`「persona already baked in」），拿不到新项目；要让运行中的管家立刻看到新项目仍只能 re-exec——但本期不主动做（留给下次自然重启）。
>
> **降级**：boot 时 `projects.json` 缺失/损坏 → 渲染为空清单、不阻塞 boot（沿用 settingsstore 坏文件范式）。

### 决策三：项目重知识由 scanner 写 `MEMORY/projects.md`，与 `projects.json` 解耦

移植 v1 `prewarm`（`scan.go`）：加项目后**异步 best-effort** 扫目录——命中 stack markers 标技术栈、有 README 摘前几行、有 `.git` 跑 `git log`（5s 超时兜底）；全失败也不阻塞 `Add`。结果用幂等 `upsertSection`（按 name 做 marker，`scan.go:210-237`）写进 `workspace/MEMORY/projects.md`，管家按需读补细节。**两份解耦**：`projects.json` = 结构化元数据（系统提示 + hotword 的真相源）；`projects.md` = 自然语言重知识。**非 git 目录照样登记成功**，git 仅作可选增强。

### 决策四：热词无独立引擎——项目名进系统提示 + 并入 ASR_HOTWORDS

不做 adapter 侧关键词→意图路由（与 v2「意图归 LLM」一致）。落两件互补的事：

1. **识别准确性**：所有登记项目名（P1 仅 name，别名数组列为后续增强）并入 `ASR_HOTWORDS`（去重、与现有合并、ASCII/CJK 校验防注入），让 ASR 更易把「Buildhub」听对。
2. **意图理解**：项目清单进 `DeviceSystemPrompt`（决策二），管家看到转写里出现项目名/近似词，就在清单里把它映射到对应项目（用途、cwd、可用 CLI），决定答还是动手。

### 决策五：CLI 工具发现——`CLIBin` 字段 + `cliReady` 状态，正面预防「BIN 空」失败

`projectstore.CLIBin` 显式记录项目专属 CLI；后端 `exec.LookPath(CLIBin)` 或 `os.Stat`（绝对路径）算出 `cliReady`。系统提示清单对每个项目标注 `[CLI: <bin> 就绪|未配置]`，并（像 `persona.go:53-61` 教 device CLI 那样）教管家操作该项目时用这个 bin。`cliReady` 在管理页直接暴露——用户加项目时若 CLI 未就绪一眼可见、当场补绝对路径。**P1 只读用户显式填的 `CLIBin`**；扫 PATH 自动登记 `bbclaw-*-bin` 列为后续（有误识别/安全面）。若真没配，管家**如实告知**「该项目的 X 工具当前没配好」而非瞎猜——是 `deviceControlSection`「命令报错就如实说、别假装成功」诚实规范的延伸。

## HTTP / UX 契约

新增独立 REST（**全部 `adminapi.LocalOnly` 包裹，loopback-only**，`main.go` newRouter 范式）。**修订 2026-06-25**：加/删**不返回 `restartRequired`**（不强制重启，§决策二）；目录选择改用**宿主机原生对话框**而非服务端目录浏览。

- `GET /v1/projects` → `{ok,data:{projects:[{name,path,source,summary,cliBin,cliReady,editable}]}}`
- `POST /v1/projects` body `{path, name?, summary?, cliBin?}` → `store.Add` 校验 → 并入 `ASR_HOTWORDS`（下次 boot 生效）→ 返回新项目（**无 restartRequired**）。scanner 写 `MEMORY/projects.md` 为后续（§决策三）。
- `DELETE /v1/projects/{name}` → 删除（**无 restartRequired**）。
- **`POST /v1/admin/pick-dir`（原生目录选择器）**：在**适配器宿主机**弹出 OS 原生「选择文件夹」对话框（macOS `osascript`「choose folder」→ `POSIX path`；Linux `zenity --file-selection --directory`），返回所选**绝对路径**。可行因为管理页 loopback-only——浏览器与适配器同机，对话框就弹在用户屏幕上。取代浏览器内渲染目录树（owner 要求；且浏览器出于安全本就不暴露所选目录绝对路径）。返回 `{path}` / `{cancelled:true}` / `{ok:false,PICKER_UNAVAILABLE}`（无原生选择器时页面降级为手动粘贴路径）。

管理页（`adapter_v2/web/admin.html`，vanilla HTML+JS）**整页重设计为 VSCode-settings 风格**（owner 反馈：要分区、保存放到需要的地方）：左侧 section 导航 + 右侧分区内容 + scrollspy 高亮，**按两套保存模型分两个色标组**：
- **即时生效 · 无需保存**（绿）：项目卡片——列出项目（名字、路径、一句用途、CLI 就绪徽标）+ 每行删除 + 底部一个「+ 添加项目目录…」按钮。**一步加入**（owner 反馈：选目录确认了就该直接加，别再要一套表单）：点按钮→宿主机原生「选择文件夹」对话框→确认即 `POST {path}`，name 由目录名自动推断，无 name/用途/CLIBin 表单（无原生选择器时降级为 `prompt` 粘贴路径）。加/删即时写入、只 toast「下次重启后生效」。`summary`/`cliBin` 后端字段保留（可经 API 设；行内编辑为后续增强）。
- **系统配置 · 需保存**（青）：原 ADR-025 设置（语音 ASR/TTS、设备、CLI、Claude 端点、云、状态），每项 label+说明+输入（VSCode 式）。**取消顶部全局「保存」按钮**；改为**底部贴附的上下文 save bar**——仅当系统配置有未保存改动时浮现「有未保存的系统配置更改 + 保存」，保存后变「已保存·重启后生效 + 重启并应用」。项目区的输入无 `data-path`，永不触发 save bar，两套模型彻底解耦。

## 否决的方案

- **A. 纯 `MEMORY/projects.md`（软指引，不碰系统提示、不 re-exec）**：改动最小、最贴 v2「claude 自管 MEMORY/」哲学，但**正好复现截图失败**——`MEMORY/*.md` 是软指引、不每轮预载，管家完全可能不读 `projects.md` 就答不知道。不保证不遗忘，否决。
- **C. driver-plugin 钩子（每 driver 注册 project-loader）**：最贴用户字面说的「插件」，但 **adapter_v2 P1 根本没有 driver 层**（直接 PTY 跑 claude TUI），引入 driver-plugin 框架巨大超纲；ADR-024 cross-driver 不共享会让切 driver 作废清单，违背「登记一次到处可见」；对非程序员更糟（要懂 driver 概念）。否决。「插件式」的真实含义落在**管理页点一下加个目录就被管家认识**，而非 driver-plugin。

## 分期

- **P1（本 ADR）**：projectstore + `/v1/projects` CRUD + 目录选择器 + `DeviceSystemPrompt` 渲染项目清单 + re-exec 落地 + scanner 写 `MEMORY/projects.md` + 并入 `ASR_HOTWORDS` + `cliReady`。范围 = 让管家**知道项目 + 能调其 CLI**。改 adapter → 需 tag 才随发布出二进制。
- **后续（独立 ADR）**：移植 v1 `list_projects`/`dispatch` MCP，让管家把任务**派进项目 cwd 跑 worker**（引入 MCP 层，超出 P1，决策已确认本期不做）；项目别名数组；扫 PATH 自动登记 CLI。

## 跨组件协议同步（CLAUDE.md「Cross-Component Protocol Sync」）

本期改动**几乎不涉及 cloud**：`projects.json`/系统提示/管理页全在 adapter 本地、loopback-only；项目清单在本地 boot 时焊进系统提示，不经 WS 协议。无新 envelope、无云端转发改动。**唯一须留意**：若后续做 dispatch（派进项目 cwd 跑 worker），butler 路由会变，届时 `homeadapter` 云端路径需同步（ADR-023 §4 parity）——本期不触及。

## 风险

- **系统提示膨胀**：项目数多会撑长系统提示。缓解：清单只渲染「名字—一句用途—CLI 状态」（每条 ≤80 字），设上限 N（建议 20，超出只列最近/最常用 N 个），重知识留 `MEMORY/projects.md`。
- **`DeviceSystemPrompt` 由静态变读盘**：须严格降级（缺失/损坏→空清单不阻塞 boot）。
- **re-exec 断连**：加项目要一次 ~1-2s 重启，设备/web 短暂断连重连——已确认接受，与 ADR-025 一致。
- **画像写冲突**：scanner 写 `projects.md` 与（未来）consolidator 写画像须串行；P1 consolidator 在 v2 未接，先用 prewarm 的 marker 段幂等写，遵守 ADR-022 原子范式。
- **注入安全**：name/hotword 进 ASR 与系统提示，须 ASCII/CJK 校验防注入；`projects.md` 写入遵守 ADR-020/022 反投毒（无绝对路径进画像、deny、0600）。

## 后果

- 管家开口即知用户有哪些项目、各自用途、专属 CLI 是否就绪——截图「问了不知道」被两层堵死（知道项目存在 + 知道并能调工具）。
- 非程序员在管理页点选目录即可让管家认识，不限 git、不用懂 driver。
- 完全落在 ADR-025 + ADR-022 既有版图内，不另起配置/记忆体系；用 re-exec 把「persona boot-once」限制天然化解。
- 代价：加项目要一次 re-exec 才生效（非当轮即时）；系统提示随项目数增长须设上限。
- 留了清晰的后续口子：dispatch（派任务进项目）、别名热词、PATH 自动发现 CLI，均为独立后续 ADR。
