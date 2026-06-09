# BBClaw 管家（Butler）—— 本地的 AI 编码调度器

> 一句话：在你自己的电脑上跑一个常驻进程，把一台对讲机式的硬件外设变成
> 「随身的 AI 编码主管」。你说话，它听懂意图、把真正的编码任务派给 worker
> 子代理去跑，再用一句话把结果讲给你。

BBClaw 管家的形态参考了 [OpenClaw](https://github.com/daboluocc/bbclaw) 的
**编排者 / 工作空间**思路：管家本身不亲自敲代码，而是一个跑在
`claude-code` 上的**调度器**，拥有一个本地「家目录」（workspace），里面装着
它的人设、长期记忆，以及它能派活的项目白名单。

---

## 1. 它是怎么工作的

```
 你（对讲机：PTT 按键 + 1.47" 小屏 + 语音播报）
        │  说一句话
        ▼
 ┌─────────────────────────────────────────────┐
 │  bbclaw-adapter（本地常驻进程）              │
 │                                              │
 │   ASR → 文本                                 │
 │     │                                        │
 │     ▼                                        │
 │   管家会话（claude-code, cwd=workspace）     │
 │     · 原生加载 workspace/CLAUDE.md 人设+记忆 │
 │     · 通过 MCP 工具派活：                    │
 │         list_projects / dispatch /           │
 │         task_status / task_result            │
 │     │                                        │
 │     ▼                                        │
 │   worker 子代理（claude-code, cwd=项目目录） │
 │     · 在目标工程里跑完整 agentic 流程        │
 │                                              │
 │   结果 → TTS → 语音播报回给你                │
 └─────────────────────────────────────────────┘
```

管家的「家」在本地磁盘：

```
~/.bbclaw-adapter/                 # 可用 BBCLAW_DATA_DIR 覆盖
└── workspace/                     # 管家会话的 cwd
    ├── CLAUDE.md                  # 人设 + 自动维护的长期记忆块
    └── MEMORY/                    # 按维度分文件的长期记忆（按需读取）
        ├── profile.md             # 用户身份档案（称呼 / 角色 / 职业）★
        ├── preferences.md         # 用户长期偏好
        ├── projects.md            # 最近在做的项目
        └── decisions.md           # 关键决策记录
```

`CLAUDE.md` 会在管家以 `cwd=workspace` 启动时被 Claude **原生加载**，所以它就是
管家的标准人设；`MEMORY/*.md` 不随之自动加载，管家按需读取对应那一份。
首次运行时这些文件由 adapter 自动建出骨架，永不覆盖你手写的内容。

---

## 2. 一键复制：让 AI agent 帮你创建并启动

把下面整段贴给任意 AI 编码助手（Claude Code / Cursor / …），或直接自己在终端跑。
它会构建二进制、写一份最小可用 `.env`、启动 adapter，并验证管家链路通了。

```bash
# ── BBClaw Adapter：一键创建 + 启动 + 自检 ──────────────────────────
cd adapter
make build                                  # → bin/bbclaw-adapter

# 1) 写一份最小可用配置（按需改 ASR/TTS 供应商与项目白名单）
cat > .env <<'EOF'
# 监听地址：本地家庭固件直接打这个口
ADAPTER_ADDR=:18080
# ADAPTER_MODE 不填即 auto：本地 HTTP 常开；配了 CLOUD_WS_URL 才同时连云

# 管家固定跑在 claude-code 上（需要 PATH 里有 `claude` 且已登录）
AGENT_DEFAULT_DRIVER=claude-code

# 管家能派活的项目白名单（list_projects / dispatch 的 cwd 来源）
# 格式 name:path,name:path —— 冒号分隔名与路径，逗号分隔多项；
# 只有白名单内的目录能被 dispatch。
BBCLAW_CWD_POOL=bbclaw:/Users/me/github/bbclaw,blog:/Users/me/github/blog

# ASR / TTS：本地联调可先用占位/mock，跑通管家文本链路
ASR_PROVIDER=openai_compatible
ASR_BASE_URL=http://127.0.0.1:1
ASR_API_KEY=dummy
ASR_MODEL=dummy
ASR_READINESS_PROBE=0
TTS_PROVIDER=mock
EOF

# 2) 启动
set -a && source .env && set +a && ./bin/bbclaw-adapter &
sleep 1

# 3) 自检：健康 + 驱动列表 + 一次真实管家轮次
curl -sS http://127.0.0.1:18080/healthz
curl -sS http://127.0.0.1:18080/v1/agent/drivers
curl -N -d '{"text":"你好，介绍一下你能帮我做什么","driver":"claude-code"}' \
  http://127.0.0.1:18080/v1/agent/message
# ───────────────────────────────────────────────────────────────────
```

跑通后，第一次真实对话时管家会自己发起初始化（见下一节）。

> 真实语音链路（接真 ASR/TTS、接云、接设备）请看 adapter 根目录
> [README](../README.md) 的「Local + Cloud」与各 `*_PROVIDER` 配置段。

---

## 3. 参考配置示例（按场景挑）

| 场景 | 关键变量 |
|------|----------|
| 纯本地家庭（local_home 固件直连） | `ADAPTER_ADDR=:18080`，不配 `CLOUD_WS_URL` |
| 本地 + 云双通道（同时服务 cloud_saas 设备） | 加 `CLOUD_WS_URL=wss://your.host/ws` + `CLOUD_AUTH_TOKEN=...` |
| 指定管家可派活的工程 | `BBCLAW_CWD_POOL=name:path,name2:path2` |
| 换数据目录（多实例隔离） | `BBCLAW_DATA_DIR=/path/to/data` |
| 关掉启动自动打开管理页 | `BBCLAW_OPEN_ADMIN=0` |
| 开启长期记忆自动蒸馏（默认关） | `BBCLAW_BUTLER_MEMORY_DISTILL=1` |

完整变量表见 [`adapter/.env.example`](../.env.example)。

---

## 4. 本地管理页：运行时加项目（`/admin`）

`BBCLAW_CWD_POOL` 是**初始播种**，但你不必为了加一个新项目去改 `.env` 重启。
adapter 启动后会**自动用默认浏览器打开** **`http://127.0.0.1:18080/admin`**
（零依赖单页；不想自动弹窗设 `BBCLAW_OPEN_ADMIN=0`），在这里可以：

- 看运行状态（健康 / 本地服务 / 已注册驱动，只读）；
- **增删管家可派活的项目目录**——点「选择目录添加」，逐级浏览本机目录选中即可，
  支持**关键字搜索**（过滤当前目录 + 递归搜子树）和**多选一次批量加入**；
  **名称自动从目录名生成**（同名自动加 `-2` 后缀），不用手填。
- **只读预览管家工作区文件**——`CLAUDE.md`（人设）与 `MEMORY/*.md`（含预热写入的
  `projects.md`），直接在页面看管家当前的人设和记忆。

> 页面是**点阵 / Nothing-style** 视觉，与 BBClaw 固件 UI 同一套设计语言
> （`design/UI_DESIGN_LANGUAGE.md`）；该风格已提炼成 `dot-matrix-ui` Claude 技能
> 供官网与其它页面复用。

> 为什么是服务端目录浏览：浏览器出于安全**拿不到**你选的目录的绝对路径，而管家派活
> 需要绝对路径。因为 `/admin` 只在 localhost、adapter 就跑在本机，索性由服务端
> （`GET /v1/admin/fs`，同样仅 localhost）列出主机目录供你点选，拿到真实路径。

加进来的目录持久化在 `<DataDir>/projects.json`（**唯一真相源**），主进程与
`mcp-server` 子进程共享同一份，**无需重启**即对管家派活生效。加目录后还会
**异步轻量扫描**该仓库（语言栈 / README 开头 / 近期 git 提交）生成摘要写进
`MEMORY/projects.md`，**预热**管家上下文——这样管家开口前就"认识"这个项目。

> **配置存哪 / `BBCLAW_CWD_POOL` 还要吗**：项目配置存在 `~/.bbclaw-adapter/projects.json`
> 这个**文件**里，不是环境变量。`BBCLAW_CWD_POOL` 只是**一次性 bootstrap**——首次运行
> 把它的项目写进文件后就被忽略，之后全在 web 页管理（包括删除原来 env 里的项目）。
> 升级时旧格式文件会自动迁移、并入 env 项目且**不丢已添加的项目**，迁移完即可放心
> 把 `BBCLAW_CWD_POOL` 从 `.env` 删掉。

### 语音说项目名识别不准怎么办

用户是语音输入，ASR 容易把项目名听错（`bbclaw` → “BB claw / 比比克劳”）。两层兜底：

- **管家模糊匹配（主）**：管家被要求先 `list_projects` 看真实项目，再按读音/拼写把
  你说的映射到最接近的项目名去派活；拿不准就报候选让你确认。provider 无关、最稳。
- **ASR 热词偏置（增强）**：实时项目名作为热词注入 ASR——OpenAI/Whisper 兼容 provider
  会下发为 `prompt` 偏置识别；火山 Doubao 大模型需预注册 boosting table，暂不内联。

> **安全（重要）**：加一个目录 = 授予管家在其中跑编码任务（含命令/文件执行）的权限。
> 因此 `/admin` 与 `/v1/admin/projects` **只接受 localhost 访问**（按请求对端地址判定
> loopback，与设备 auth token 解耦），绝不暴露到局域网或云。`BBCLAW_CWD_POOL` 里
> 用 env 定义的项目属操作员配置，不能在管理页删除。

```bash
# 也可直接用 API（同样仅 localhost）：
curl -sS http://127.0.0.1:18080/v1/admin/projects                 # 列出
curl -sS -d '{"name":"blog","path":"/Users/me/code/blog"}' \
  http://127.0.0.1:18080/v1/admin/projects                        # 添加
curl -sS -X DELETE http://127.0.0.1:18080/v1/admin/projects/blog  # 删除
```

---

## 5. 初始化：用户激活后，对话式录入身份

管家第一次跟你说话时，它的 `MEMORY/profile.md` 还是
`STATUS: uninitialized`。这份「初始化」逻辑直接写在管家的启动系统提示
（`workspace/CLAUDE.md` 人设）里，无需任何按键或表单——纯对话完成：

1. **先办正事**：你开口提的请求，管家先回应；
2. **顺势发起**：办完后用一句自然的话问最少必要的几项——
   - 「我该**怎么称呼你**？」
   - 「你的**角色 / 职业**是？」（用于调整默认做法）
   - 「现在**主要在忙什么**？」
   一次不全问，可以分几轮慢慢补；
3. **写入档案**：你回答后，管家把信息写进 `MEMORY/profile.md` 并把标记改成
   `STATUS: initialized`，之后正常用这个称呼叫你、按你的角色调整默认；
4. **可跳过**：你说「不用了 / 以后再说」，管家标记 `STATUS: skipped`，不再追问。

> 设计要点：onboarding **永远让位于你当下的请求**，绝不打断紧急任务。
> 身份档案由管家手写维护，**不进**自动蒸馏 / 整理循环——所以你口述的称呼
> 与角色不会被后续提炼出的笔记覆盖。

一段典型的初始化对话：

```
你   ：帮我看看 bbclaw 这个项目最近的提交
管家 ：最近三条都是固件 UI 的点阵化改造…（先把活办了）
       对了，我还不知道该怎么称呼你——你希望我怎么叫你？
你   ：叫我老周就行，我是这个项目的负责人
管家 ：好的老周。（写入 profile.md：称呼=老周；角色=项目负责人；
       STATUS=initialized）以后默认按「负责人视角」给你结论。
```

之后每次对话，管家都会带着「你是谁、怎么称呼、在忙什么」的上下文，
不必每次重新自我介绍。

---

## 6. 相关代码位置

| 关注点 | 文件 |
|--------|------|
| workspace 目录与 `CLAUDE.md` / `MEMORY/` 脚手架 | `internal/workspace/workspace.go` |
| 管家工厂人设（含初始化指令） | `internal/workspace/persona.go` |
| 每轮注入的设备约束系统提示 | `internal/butler/persona.go` |
| 管家会话编排（LOCAL/CLOUD 共用骨架） | `internal/butler/engine.go` |
| 派活 MCP 工具（list_projects / dispatch / …） | `internal/butlermcp/server.go` |
| 长期记忆管线（蒸馏 / 整理 / 抗投毒） | `internal/butler/memory/` |
| 可变项目池（持久化 / env 播种 / 实时重读 / 自动命名） | `internal/projectstore/store.go` |
| 本地管理页 + admin 接口 + 目录浏览（localhost-only） | `internal/httpapi/admin.go` · `admin.html` |
| 加目录后的轻量预热扫描 | `internal/prewarm/scan.go` |
| 启动后自动打开浏览器 | `cmd/bbclaw-adapter/openbrowser.go` |
| ASR 热词偏置（Whisper `prompt`） | `internal/asr/asr.go` |

设计依据：ADR-021（管家工作空间）、ADR-022（多维度长期记忆）。
