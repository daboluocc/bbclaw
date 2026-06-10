# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **管家长期记忆在 cloud-relay 模式下完全不写入（ADR-021 §4）**：记忆写入侧
  (`Memory`)、派活 ring (`DispatchRing`) 与 recorder 此前只接到**本地 ingress** 的
  butler engine 上,**两条 cloud-relay 路径**(`homeadapter` 语音 + agent-proxy)的
  `butler.Deps` 都没接。设备经云端中转时(远程设备的常态),每轮 turn 走的是 cloud 路径
  → `d.Memory==nil` → `RecordTurn` 从不触发 → CLAUDE.md 的 `BBClaw-managed` 收件区
  一直空、`MEMORY/*.md` 一直是模板,派活历史也录不进去。现在 memory/dispatch infra 在
  `run()` 里**只建一次**并同时接到本地 + cloud-relay 两条 engine 上(单 home adapter =
  单 home,共用一个 Writer 是对的;多租户隔离是云端后端的事,不在本仓库)。turn 本身一直
  是成功的(errors=0),所以是纯接线遗漏,不是 turn 失败。
  注:`MEMORY/*.md`(consolidation 产物)仍按设计默认关闭——其两个缺陷未修前,真正生效的
  长期记忆是 CLAUDE.md 收件区;开了 `BBCLAW_BUTLER_MEMORY_CONSOLIDATE` 反而会把收件区
  抽进没人读的单数命名死文件。

### Added
- **Adapter 管理页「日志」tab + 持久化日志文件（ADR-025）**：新增 `GET /v1/admin/logs`
  （loopback-only）返回内存环形缓冲里最近 ~1000 行运行日志，管理页独立「日志」页实时
  展示（3s 轮询、自动跟随底部、暂停/刷新），用户不必再盯着二进制 stdout。日志同时
  **持久化到 `~/.bbclaw-adapter/adapter-runtime.log`**（即 `$BBCLAW_DATA_DIR/...`，
  任何启动方式都写，超 16MB 滚动到 `.1`，0600），页面顶部显示该绝对路径 + 复制按钮，
  方便 AI/CLI 直接 tail。`obs.Logger` 增加 ring buffer 与 `Tee()` 多路输出（stdout 不变）。
- **设备身份只读展示**：`GET /v1/admin/settings` 增加 `derived` 块（解析后的
  `home_site_id`、构建 `version`、`log_file`）。「设置」页新增只读「设备身份」卡片展示
  这些系统生成值，不再当作可填字段。

### Changed
- **Adapter 管理页重排导航（ADR-025）**：默认落地页从「系统配置」改为
  **「个人对话」**（打开 `/admin` 直接看聊天记录），设置相关页后置。原本独立的
  **「系统配置」「AI 配置」两页合并为单页「设置」**（部署模式 + 设备身份 + 高级设置，
  下接第三方端点 / 项目白名单 / ASR-TTS，顺序排在同一页），顶栏 tab 变为
  **个人对话 / 设置 / 日志 / AI 数据文件** 四项。老书签 `/admin/system` `/admin/ai`
  `/admin/projects` 自动落到「设置」。部署模式保存后会重挂 AI 面板，让本地/云端切换
  即时反映到下方 ASR/TTS 区是否显示。
- **重启开关移到右上角**：去掉原「配置已保存→立即重启」横幅，改为常驻顶栏右上角的
  「重启」按钮；有未生效的已保存改动时按钮高亮 + 脉冲点提示「重启生效」。
- **「设置」页收掉非用户配置项**：删掉 OpenClaw 网关配置区——OpenClaw 与
  claude-code / codex 一样只是一个**驱动**（在「驱动」区出现），不再当默认配置；
  删掉可编辑的 Home Site ID 输入框（改为上方只读展示）；高级设置「云端 relay」精简为
  仅「自建云」WS 地址 + Auth Token（默认指向生产云，开箱即用）。

## [0.4.14] - 2026-06-10

### Added
- **Adapter 配置 Web 化（ADR-025）**：把原本只能写 `.env` 的运行配置（ASR/TTS、
  第三方 Claude 端点、cloud relay、OpenClaw 网关、音频留存）搬上本地管理页，落
  `~/.bbclaw-adapter/settings.json`（`.env` 退化为首启一次性 seed，之后页面是真相）。
  管理页拆成四页：**系统配置 / AI 配置 / 个人对话 / AI 数据文件**。改动「保存即持久化 +
  一键重启生效」（`POST /v1/admin/restart` 原地 re-exec）；驱动/模型/项目仍即时生效。
- **默认 cloud、本地免配语音**：本地语音管线（LAN 直连 ASR/TTS）改为 opt-in
  (`topology.local_voice_enabled` / `BBCLAW_LOCAL_VOICE`)。云端模式下 ASR/TTS 由云端
  完成，本地空 `.env` 即可启动；关闭时 `/v1/stream/*`、`/v1/tts/*` 回 501。既有完整
  语音配置自动保留（首启按 env 是否完整自动判定）。新增 `GET/PUT /v1/admin/settings`
  （loopback-only，明文读写）。
- **管理页按部署模式自适应简化**：系统配置页改为单选「☁ 云端 / ⌂ 本地」一个选择驱动
  全页——云端模式本地零配置（cloud relay / OpenClaw / 音频留存收进「高级设置」折叠），
  本地模式才在 AI 配置页显示 ASR/TTS。语音未配完整时**优雅降级**（保存 200 +
  `voice_incomplete` 提示、启动 WARN + 语音回 501）而非报错/崩启动，避免「切本地模式
  才出 ASR 表单、但没填 ASR 又切不过去」的死锁。

### Changed
- **底栏扫描条改为 3×N 点阵带**：单排扫描条升级为 3 行点阵小屏（dot 4 / pitch 7 /
  vpitch 5），彗星变成"整列"白头青尾的全高扫描，更像一块迷你点阵屏；颜色/速度随状态
  联动不变。
- **底栏点阵带按状态切 motif**：不再只有单一 sweep，改为随对话状态切换不同动画——
  处理=扫描 sweep / 聆听=声波 vu / 说话=行波 wave / 错误=红脉冲 pulse / 待机=呼吸
  breathe。与官网 daboluo.cc/style 的 `dot-matrix-anim.js` motif 库同源。
- **状态栏精简 + 电池可读性**：顶栏去掉占地的 WiFi SSID 文字（信号格已表达"有 WiFi +
  强度"），腾出的空间让电池重新露出；电池常态填充改冷白（更像标准电池，充电绿/低电红
  不变），并在图标左侧加 `NN%` 数字（低电红、充电绿）。

### Fixed
- **adapter release CI 因 prewarm 测试清理竞态失败**：加目录触发的 `prewarm.RecordAsync`
  异步写 `MEMORY/projects.md`，会在测试 `t.TempDir` 清理后落盘 → `directory not empty`
  让 `TestAdminProjectsAddListDelete` 失败、挡住发版。给 prewarm 加 `WaitGroup` + `Wait()`，
  测试 helper 在临时目录拆除前 await 在途扫描。

### Removed
- **移除对话页右上角字符小人（buddy）**：agent 聊天主题 `bb_theme_buddy_anim` 过去在
  transcript 右上角浮一个 `(^_^)` 字符表情 + mood 小窗（九态动效）。角色状态已由顶部
  状态栏图标 + 底栏点阵扫描条表达，右上角小人冗余且遮挡正文，故移除。transcript 聊天
  消息流、录音遮罩、历史回放等主题功能保持不变；`set_state` 仍保留（驱动顶栏图标语义）。

## [0.4.13] - 2026-06-10

### Changed
- **WiFi 配网改为独立全屏页**：SoftAP 配网模式过去把 AP 的 SSID/密码/IP 塞进一个
  对话气泡（`bb_display_show_chat_turn`）显示，拥挤且与聊天内容混淆。改为独立的点阵
  配网页 `bb_page_apconfig`：左侧点阵 WiFi 广播图标（向外涟漪），右侧"热点/密码/打开"
  三步加入指引（CJK 文字渲染）。首启无凭据和运行中 WiFi 掉线两条进入路径都已切换。
  详见 design/STATE_MACHINE.md §3.5.2。
- **对话页底栏改为点阵扫描条**：ACTIVE 对话页底栏过去是 `[B] cwd | mem:N+M` 文字双格，
  现改为一条 320px 点阵"小屏幕"——白头青尾的彗星在一排 ghost 点上 L↔R 往返扫描
  （Knight-rider），颜色与速度随状态联动：待机青慢 / 聆听青中 / 处理青快 / 错误红
  （NO WIFI、WIFI ERR、AUTH）。cwd / mem 统计改由锁屏页 footer 承载，派发进度仍叠加
  在顶栏状态文字；记忆/cwd 状态与 API 不变。详见 design/UI_DESIGN_LANGUAGE.md §3。
  模拟器 headless 导出新增逐帧推进（`lv_tick_inc`），动画类预览不再停在 t=0。

## [0.4.12] - 2026-06-10

### Changed
- OTA 验证版本:相对 v0.4.11 无代码变更,仅用于在真机上走一遍修复后的完整 OTA
  链路(下载 → 进度页 → 重启 → 自报新版本 → device_id 稳定不丢配对 → 收敛)。

## [0.4.11] - 2026-06-10

### Fixed
- **OTA 升级后设备变"新设备"**: `device_id` 原为 `BBClaw-<固件版本>-<MAC>`,版本号从
  tag 注入后,每次 OTA 都会改变 `device_id` → 云端当作全新设备要求重新配对
  (`claim_required`、配置回默认)。改为 `BBClaw-<MAC>`,与固件版本无关、跨 OTA 稳定。
  一次性影响:现有已配对设备的 id 会变一次,需重新认领一次,此后永久稳定。(#123)

## [0.4.10] - 2026-06-10

### Fixed
- **OTA 无限重刷循环**: 固件版本(`esp_app_desc.version`)此前硬编码在
  `CMakeLists.txt` 的 `project(... VERSION 0.4.1)`,OTA 后设备永远自报 0.4.1 <
  云端 active → 反复重新下载同一版本。改为版本从 `version.txt`(CI 写入发布 tag)
  / `git describe`(本地)注入。(#122)
  *(v0.4.9 的 octal PSRAM 修复已在真机验证:能正常启动,仅版本号循环。)*

## [0.4.9] - 2026-06-10

### Fixed
- **OTA 变砖修复(关键)**: 发布固件改用 `firmware/sdkconfig.bbclaw.latest`(OCTAL PSRAM
  + bbclaw 板 + cloud_saas + 生产云 URL)构建。此前 CI 走 `sdkconfig.defaults`(QUAD PSRAM
  + breadboard + local_home),发的固件 OTA 到八线 PSRAM 的 bbclaw PCB 会
  `wrong PSRAM line mode` → `Failed to init external RAM` → boot loop。OTA 链路本周才修通,
  v0.4.8 是第一个真正被 OTA 的 CI 构建,因此首次暴露。(#120 #121;默认板也改为 bbclaw)

### Added
- **开机动画下方显示当前固件版本**(`bb_ota_get_current_version`),便于确认实际启动的
  构建 / OTA 分区。(#119)

## [0.4.8] - 2026-06-10

### Added
- **设备端音量调节（固件 Settings）**：Settings 新增 `Volume` 行 + 调节态，UP/DOWN
  ±5% 实时生效、点阵进度条，OK/BACK 保存返回（#112/#113）。开机应用已保存音量、云端
  心跳不再在开机/同值时回灌覆盖本地选择（#114）。
- **语音调节设备音量**：管家 agent 经 `bbclaw-adapter device set-volume <pct> --device <id>`
  CLI 调云端写配置 → 下发设备;每轮 butler sysprompt 注入当前 deviceId + CLI 用法（#116/#117）。
  *(云端 `POST /v1/devices/{id}/config` 接受 home-adapter Bearer + camelCase + 双 store 下发，
  server-side 部署。)*
- **OTA 升级点阵进度页**（`bb_page_ota`）：下载时屏幕显示点阵进度条 + `UPDATING NN%` +
  目标版本，完成显 `REBOOTING`，替换原静默下载（#118）。
- **固件历史按会话段展示**：解析 adapter 下发的 `timestamp`，设备端历史按时间分段渲染;
  离线时提示「仅本地缓存」（#110/#111）。

### Fixed
- **设置里调音量/切 TTS 按 OK 必崩重启**：NVS 写发生在 PSRAM 栈的 stream_task 上，写
  Flash 冻结 cache → assert 重启;改为投递到内部 RAM 栈任务持久化（#115）。
- **自动 OTA 链路断裂（云端）**：release workflow 只传 flash-bundle（bundle store），但
  `/v1/ota/check` 读 firmwares store，从不联动 → 永远无更新。flash-bundle 上传时同时把 app
  镜像注册为 active OTA firmware;`ParseVersion` 容忍 `v` 前缀（server-side 部署）。

## [0.4.7] - 2026-06-09

### Added
- **管理页改为独立 Vue SPA（`adapter/web`）+ 对话记录页**：把原内嵌 vanilla 单页重写为
  Vue3+Vite+TS 工程，构建产物 `internal/adminui/dist` 提交进仓库并 `go:embed` 打进
  二进制 serve `/admin`（**单二进制不变、发布流水线零改动、不需要 Node**；`make web`
  仅在改前端时重建）。新增**对话记录**标签页：会话列表 + 消息气泡流（user/assistant）
  + 派活任务卡片，**按对话时间间隔自动分段**（>30min 插入时间分割线）；消息接口补
  `timestamp` 字段（`agent.Message`，从 claude transcript 解析）。参考 agent_room 的
  记录展示模式，沿用点阵风格。新增 localhost-only 只读会话接口 `/v1/admin/sessions[/{id}/messages]`、
  `/v1/admin/dispatch/recent`。原 `internal/httpapi/admin.html` 退役。
- **管理页升级：工作区文件预览 + 目录关键字搜索 + 多选批量加入 + 点阵风格**：
  `/admin` 现可**只读预览**管家工作区文件（`CLAUDE.md` 人设 + `MEMORY/{profile,
  preferences,projects,decisions}.md`，白名单防任意读，新增 `/v1/admin/workspace-file[s]`）；
  目录选择器加**关键字搜索**（过滤当前目录 + 服务端递归搜索 `/v1/admin/fs/search`，
  限深限量、跳过 `node_modules`/`.git` 等）与**多选批量加入**；整页重做为**点阵 /
  Nothing-style** 视觉（对齐 `design/UI_DESIGN_LANGUAGE.md` token）。
- **新增 Claude 技能 `dot-matrix-ui`**（`.claude/skills/dot-matrix-ui/SKILL.md`）：把点阵
  设计语言提炼为可复用的 Web 落地版（token、CSS 变量、组件配方、do/don't），供 adapter
  页面与 daboluo.cc 官网统一复用，与固件 `bb_ui_theme.h` 同源。
- **启动后自动打开管理页**：本地 HTTP 起来后自动用默认浏览器打开 `/admin`（跨平台
  open/xdg-open/start，headless 失败静默非致命，`BBCLAW_OPEN_ADMIN=0` 可关）。
- **管理页改为目录选择器**：浏览器拿不到本地绝对路径，故新增服务端目录浏览
  `GET /v1/admin/fs?path=`（仅 localhost）逐级浏览主机目录来选项目目录；项目**名称
  从目录名自动派生**（去重加 `-N` 后缀），管理页不再要求手填名称（`store.AddPath`）。
- **ASR 项目名识别准确性（两层）**：① 管家层——persona 指令让 LLM 先 `list_projects`、
  再按读音/拼写把用户语音里的项目名模糊匹配到真实项目再 dispatch（provider 无关、
  主防线）；② ASR 层——`asr.Metadata.Hotwords` 注入实时项目名，OpenAI/Whisper provider
  作为 `prompt` 偏置下发（Doubao bigmodel 需预注册 boosting table，暂为文档化 no-op）。
- **本地轻量 Web 管理页（`/admin`）+ 运行时项目目录管理（web-first）**：adapter 启动后在
  `/admin` 提供一个零依赖单页（embed，无构建步骤），展示运行状态（健康 / 本地服务 /
  已注册驱动，只读）并支持**增删管家可派活的项目目录**。新增 `internal/projectstore`
  持久化项目池（`<DataDir>/projects.json` 为**唯一真相源**，原子写、mtime 变化时重读），
  主进程与 `mcp-server` 子进程共享同一文件 → 加目录**无需重启**即对管家派活生效。
  **`BBCLAW_CWD_POOL` 改为一次性 bootstrap 播种**：首次运行写入文件后即被忽略，之后
  所有项目（含原 env 项目）都在 web 页可增删；旧版 `{"added":...}` 文件会在启动时
  自动迁移为新格式并把 env 项目并入，**不丢已添加的项目**，迁移后即可从 `.env` 删掉
  `BBCLAW_CWD_POOL`。加目录后异步轻量扫描仓库（语言栈 / README / 近期 git 提交）
  生成摘要写进 `MEMORY/projects.md`，**预热**管家上下文（`internal/prewarm`）。
  安全：`/admin` 与 `/v1/admin/*` **仅限 localhost**（按对端地址判定 loopback，与设备
  auth token 解耦），因为加目录等于授予管家在该目录跑命令/文件执行的权限。
- **管家首次激活的对话式身份初始化（onboarding）**：新增 `MEMORY/profile.md`
  身份档案维度（怎么称呼用户 / 角色 / 职业），并在管家启动人设
  （`workspace/CLAUDE.md`）注入初始化指令——`STATUS: uninitialized` 时管家先办
  用户当下请求、再顺势用一两句话录入身份，写入后置 `initialized`，用户拒绝则
  `skipped`，绝不打断紧急任务。身份档案由管家手写维护，**不进**自动蒸馏 / 整理
  循环，避免被提炼笔记覆盖（`internal/workspace/`）。
- **adapter 独立介绍文档** [`adapter/docs/butler.md`](adapter/docs/butler.md)：管家
  工作空间模式说明、一键复制给 AI agent 创建并启动、参考配置示例、初始化对话流程；
  另附官网 Adapter 页面内容草稿 `adapter/docs/website-adapter-page.md`。README 顶部
  加入指引链接。

## [0.4.6] - 2026-06-07

### Changed
- **全 UI 统一点阵设计语言（dot-matrix / Nothing-style，design/UI_DESIGN_LANGUAGE.md）**：开机动画 / 网络连接页 / 待机时钟三页确立的视觉语言推广到全机。新建唯一真相 token 表 `design/UI_DESIGN_LANGUAGE.md` + `firmware/include/bb_ui_theme.h`，11 个 UI 源文件的本地调色板全部收敛为 token 引用，禁止裸 hex 色。关键视觉变化：
  - **底色统一**：旧 `0x0a0e0c`（偏亮）全部换 `BB_UI_BG 0x070b0e`——页面切换不再有底色跳变；状态栏/底栏暖绿 `0x8fbcac` 与旧暖灰次级文字统一为冷蓝灰 `BB_UI_TEXT_DIM 0x6e8a93`
  - **LOCKED 页点阵重构**：锁形改点阵画法（7 点 shackle 弧 + 5×4 点 body + 青色 keyhole 呼吸点），密语验证时呼吸加速、失败时 body 闪红一拍；顺手修复旧布局 title y=208 / hint y=230 在 172px 屏上**越界不可见**的 bug——文案移到锁形右侧块内
  - **录音 VU 点阵化**：chat 录音遮罩（7×5）与语音 speaking 视图（10×5）的连续条 VU 全部改为点阵柱，bottom-up 点亮、峰值点 voiced 时青色闪——与开机扫列同节拍语言；平滑攻衰减逻辑保留
  - **聊天气泡单色化**：assistant 蓝色 `0x4a9fd8` 弃用，改 ghost 深灰面 + 冷白文字；用户气泡保留青色 30%——左右对齐 + 色块强弱区分说话者
  - **buddy 九态全收敛单色+青**：face 暖奶油/mood 暖棕/attention 金黄/celebrate 粉全部映射到冷白/冷蓝灰/青三色，状态差异交给既有动效表达
  - **列表选中态统一**：Settings / 任务列表 / chat picker 三处选中行统一为「ghost 行面 + 青色左缘 3px 竖条 + 冷白文字」（替换旧的青底白字/青底黑字混用）
  - 模拟器新增 `--mode locked`（支持 `--status "VERIFY ERR"` 等预览验证态）

## [0.4.5] - 2026-06-07

### Removed
- **设备端 CWD picker + new-session 死代码整链清理**：ADR-021 v2 砍掉 session picker 后，CWD picker（issue #30）唯一入口（session picker 的「+ 新建 session」行）随之断链，整条链路不可达：CWD picker overlay UI 与按键路由、设备端 new-session worker（`spawn_new_session_task` 等）、客户端 API `bb_agent_create_session` / `bb_agent_list_cwd_pool`（`POST /v1/agent/sessions` / `GET /v1/agent/cwd-pool` 设备侧调用）、`bb_display_set_cwd_name` 及 `format_relative_time`。设备 turn 已由 adapter `EnsureButler` 强制路由管家会话，设备端不再创建 session。共删 ~650 行（6 个源文件 + 3 个头文件）。

### Fixed
- **开机动画没播完就被硬切**：splash dismiss 只看墙钟 `BBCLAW_BOOT_SPLASH_MIN_MS`（2600ms），但扫列动画跑在 LVGL task 的 `lv_timer` 上，boot 期间音频 init / boot wav 播放会饿 LVGL task，节拍落后墙钟 → 到点时列没扫完/下划线没长完就被销毁。修复：新增 `bb_page_boot_anim_done()`（收尾 tick 自删 timer 即 done），`bb_radio_app_start` 在 MIN_MS 补足后继续 50ms 轮询等动画真正收尾，上限 `BBCLAW_BOOT_SPLASH_ANIM_GRACE_MS`（默认 2000ms）防 LVGL 卡死无限等。设计文档 `STATE_MACHINE.md` §3.5 同步。
- **待机时钟 SNTP 同步前全黑**：`bb_page_standby_refresh_clock` 对 `"--:--"`（时间未就绪）解析不到数字 → 4 个 slot 全 ghost ≈ 黑屏。兜底：无数字时各 slot 渲染居中横杠（5×7 中间行 3 点亮），任何 fallback 路径下时钟页都有内容。
- **麦克风近场削顶导致云端 ASR 识别为空**：INMP441 软件增益 8x 对近场/稍大声说话把波形顶到 `INT16_MAX`（pcm diag `max=32767 clipped=17`、正向严重削顶、负偏分布），失真音频上行后云端 ASR 返回空文本（`phase=asr text= (empty)` → `agent_chat: empty transcript`）。`BBCLAW_AUDIO_INMP441_GAIN_NUM` 8→4，保留 ~2 bit 动态余量同时对 INMP441 偏低的原始电平仍够响。
- **失效逻辑会话每次开机刷 `SESSION_NOT_FOUND`**：设备重启后从 NVS 复用上次逻辑会话 id，若该会话已在 adapter/cloud 侧失效，拉历史返回 HTTP 400 `SESSION_NOT_FOUND`，旧逻辑当通用失败处理、本地 id 永不清除 → 每次开机复现、历史区常空。修复：`bb_agent_load_messages` 对 `SESSION_NOT_FOUND` 返回独立的 `ESP_ERR_NOT_FOUND`，`on_history_fetch_done` 据此自愈——清 NVS 会话（`bb_session_store_save(drv, "")`）+ 重置内存 `session_id` + 清残留 transcript/cache，下个 turn 由 adapter 新建会话并自动存回 NVS。
- **运行时内部 RAM 耗尽 → websocket task 创建失败、语音流挂**（`Error create websocket task`，internal_free=27KB / largest=7.6KB < 8KB task 栈）：bbclaw 板配置用 CLIB malloc + `SPIRAM_MALLOC_ALWAYSINTERNAL=16384`，每个小 lv_obj 都落内部 RAM——点阵 UI 风格（待机页 140+ dots、开机动画 210+ dots、聊天气泡）数百个小对象把内部堆吃碎。修复双管齐下：(1) 新增 `bb_lvgl_mem.c` 自定义 LVGL 分配器（`CONFIG_LV_USE_CUSTOM_MALLOC`），所有 LVGL 分配 PSRAM 优先、内部兜底，内部 RAM 留给 task 栈和 WiFi/I2S DMA；(2) `SPIRAM_MALLOC_RESERVE_INTERNAL` 32K→64K，可去 PSRAM 的 malloc 更早转移。`sdkconfig.defaults` / `sdkconfig` / `sdkconfig.bbclaw.latest` 三处同步（顺带修正 committed sdkconfig 与 defaults 的 BUILTIN/CLIB 漂移）。
- **开机动画淡出与 WiFi init 并发导致 ESP_ERR_NO_MEM boot loop**：splash dismiss 原为 350ms 整屏 opa 淡出（异步）——parent-opa 让 LVGL 经临时全屏合成层渲染，且淡出窗口与 `esp_wifi_init` 重叠，WiFi 的 10×1600B 静态 RX DMA 缓冲拿不到内部 RAM（`wifi:malloc buffer fail` → `Expected to init 10 rx buffer, actual is 5`）→ `ESP_ERROR_CHECK` abort 重启循环。修复：dismiss 改为同步硬切销毁（splash 与待机/锁屏底色几乎同色，视觉无碍），返回即全部资源释放；另在 WiFi init 前加 `log_heap_snapshot("pre_wifi_init")` 水位快照便于复现定位。
- **buddy 九态表情动画全部失效（Phase 7 死代码复活）**：Phase 7 把 chat overlay 改透明时移除了主题自有 topbar，face/mood label 置 NULL 但注释声称"已迁到 ACTIVE 视图"——实际从未迁移，`apply_state_anim()` 等 ~300 行动画代码因宿主对象为 NULL 永远 early-return，任何按键/PTT 引发的状态切换都无表情动画。修复：在 transcript 右上角重建 ~96×38 半透明圆角 buddy chip（face+mood 两行），九态动画（sleep 呼吸/idle 浮动/busy 点点/speaking 摇摆/heart 心跳/listening 浮动+脉冲/dizzy 抖动/attention 变色/celebrate 弹跳）整体复活；录音遮罩 show 时仍 move_foreground 盖住 buddy 避免 LISTENING 双重提示。设计文档 `STATE_MACHINE.md` §3.1/§3.3 同步。
- **回复语音没播完就被切到待机页**：CHAT→STANDBY 的 30s 空闲判定只看 voice-PTT 管线的 `s_tts_playback_active`，而 agent 回复朗读走的是 `bb_ui_agent_chat.c` 里独立的 `tts_playback_task`（turn 结束 busy=0 后仍逐句合成+播放，喇叭输出显著滞后于 turn 生命周期），长回复播到一半就被 idle 定时器拉去待机页。修复：空闲判定改为「设备空闲 + 喇叭空闲」三重检查——`s_tts_playback_active`（语音管线）+ 新增 `bb_ui_agent_chat_tts_speaking()`（chat 朗读任务存活，含合成等待期）+ `bb_audio_is_playback_active()`（I2S TX 通道电平，兜底提示音等）。喇叭活跃期间持续刷新活动时间戳，待机倒计时从**语音完整结束**那一刻起算。
- **待机页直接按 PTT 无「正在聆听」动画/震动反馈**：`stream_task` 的 STANDBY→CHAT 唤醒分支先消费了 PTT 版本号再 `continue`，下一轮循环只剩 `s_ptt_pressed` 电平驱动的 arm 录音路径，唯一触发 LISTENING 状态 + 录音波形遮罩 + 按下震动的 `chat_voice` 边沿分支被整个跳过——录音在跑但屏幕毫无反馈。修复：`agent_chat_enter()` 成功后不再吞边沿，落穿到同一迭代的 `chat_voice` 分支，行为与已在 CHAT 页按 PTT 完全一致（含 busy/adapter-offline 防护）。
- **固件编译失败**：(1) `bb_ui_agent_chat.c` 一处旧注释缺 `*/` 结尾，与下一行注释嵌套触发 `-Werror=comment`；(2) `bb_lvgl_display.c` / `bb_page_locked.c` 底栏 `"mem: %d+%d"` 在 int 极值下可能截断 24 字节 buffer 触发 `-Werror=format-truncation`，inbox/profile 显示值钳制到 999。

### Added
- **网络连接点阵动画页 (`bb_page_netconn.c`, STATE_MACHINE.md §3.5.1)**：待机页是点阵时钟，SNTP 同步前没有内容可显示，而 WiFi 连接最长 30s+/SSID——开机动画硬切后用户面对近乎黑屏。新增网络连接页无缝接管：同点阵语言（5px dot / 同 palette）的 WiFi 弧形图标（底部基点 + 3 层同心点弧，共 16 dots）自下而上逐层点亮循环（420ms/层，最新层青色闪、下一拍沉淀冷白），图标下方实时显示正在尝试的 SSID（`WiFi <ssid>`，每 tick 轮询 `bb_wifi_get_active_ssid()`，多 slot 重试时自动跟随）；连上后弧全亮定格青色、标签换 `SYNC TIME`，等到 `bb_wall_time_ready()`（或连上后超时 `BBCLAW_NETCONN_SYNC_TIMEOUT_MS`，默认 10s）自销毁，露出**已有时间**的待机时钟。provisioning / wifi 失败路径由 `bb_radio_app_start` 显式 dismiss 让位 AP info / 错误显示。show/dismiss 与 splash 同样同步硬切不做 fade（NO_MEM 教训）。开关 `BBCLAW_NETCONN_PAGE_ENABLE`（默认 1）。
- **诺基亚式像素点阵开机动画 + 语音协同 (`bb_page_boot.c`)**：开机后在 `lv_layer_top()` 全屏深色底铺出 "BBCLAW" 六字母 5×7 ghost 点阵（复用待机页点阵语言：dot 5px / pitch 9px），逐列扫亮（35ms/列，最新列青色高亮、下一拍沉淀冷白），扫完青色下划线从左向右生长收尾，结束整体淡出露出底层视图。开机语音（boot wav）延迟到扫列完成后才播：`bb_radio_app_start` 在 SPK TEST 前等到动画开始 ≥`BBCLAW_BOOT_SPLASH_VOICE_DELAY_MS`（默认 1150ms），播完后不足 `BBCLAW_BOOT_SPLASH_MIN_MS`（默认 2600ms）补足再淡出。开关 `BBCLAW_BOOT_SPLASH_ENABLE`（默认 1）。设计文档 `STATE_MACHINE.md` §3.5。
- **管家记忆「沉淀引擎」— 收件箱归档进 MEMORY 多维画像并清空 (ADR-022 v1, #92)**：在 ADR-021 §4 per-turn distill（收件箱 append）之上新增**第二层「沉淀」**：后台把 workspace `CLAUDE.md` 托管段（收件箱）归档进 `MEMORY/*.md` 多维画像并清空收件箱，使 4KB 从「FIFO 静默丢失硬上限」降级为「整理缓冲」。默认 **off** 灰度、LOCAL-only。
  - **触发器（四类 + cooldown，`trigger.go`）**：阈值（收件箱 ≥75% `maxBytes`）/ 空闲（`idleGap`，默认 5min）/ 兜底（`maxGap`，默认 6h）/ per-key cooldown（默认 10min）。决策是纯函数 `decideTrigger`（cooldown 门 → 阈值 → 空闲 → 兜底，空收件箱永不触发），I/O 与决策分离便于表驱动测试。挂在 `MemoryWriter.RecordTurn`（engine.go:538）打 turn 末时间戳（单次轻量赋值，保持非阻塞契约）；阈值在 worker 每轮 append 后同步检查，空闲/兜底由轻量 ticker 周期驱动。
  - **整理引擎（`consolidator.go`）**：读收件箱（`ManagedBlock` 快照）+ 现有 `MEMORY/*.md` → `claude -p` Haiku 归类成 JSON 对象（preference/project/decision 三维度）→ 经 `IsPoisoned` **双过滤** + 每维度上限（默认 30）裁剪 → **0600 原子写**各 `MEMORY/<dim>.md`（无绝对路径，目录防御式自建 `filepath.Dir(CLAUDE.md)/MEMORY`，与 #91 解耦）→ **仅全部写盘成功才清空收件箱**；任一步失败全吞(log)、**绝不清空**（下轮从仍在的收件箱重derive，收敛）。
  - **整理 prompt 规则（`summarizer_claude.go`）**：合并 / 去重 / **新覆盖旧（真删）**（每次对 `MEMORY/<dim>.md` 整文件重写，被取代的旧事实物理删除）/ 剔除过期 / 丢指令性内容 / 每维度上限。
  - **读-清空与 append 竞态**：清空采用**快照感知清除**——只移除「快照集合里出现过的行」，沉淀期间（LLM 调用秒级）新 append 进收件箱的行不在快照内、予以保留。即使未来 consolidation 移出 worker 并发执行仍正确。复用同一**并发=1 worker**：distill append 与 consolidation 在同一 worker 串行排队（distill 走 `ch`，consolidation 走独立信号 + worker `select`，ticker 满即丢不互相饿死）。
  - **spike 结论**：`claude -p` Haiku 大 JSON 稳定性采用与 `parseItems` 同源的**容错切片**（首 `{` 到末 `}`）而非 `--output-format json`（后者多一层信封）；解析失败 → 不清空收件箱（下轮重试），脏条目 → `IsPoisoned` 拦截 + 每维度上限封顶。本仓 CI 无 claude CLI/API key，真链路冒烟留集成测试（`BBCLAW_BUTLER_LIVE`）。
  - **env 门控**（沿用 `BBCLAW_BUTLER_MEMORY_*` 约定）：`BBCLAW_BUTLER_MEMORY_CONSOLIDATE`（默认 off，需 `_DISTILL` 也开）/ `_CONSOLIDATE_THRESHOLD`（0.75）/ `_CONSOLIDATE_IDLE`（5m）/ `_CONSOLIDATE_MAXGAP`（6h）/ `_CONSOLIDATE_COOLDOWN`（10m）/ `_CONSOLIDATE_MAXPERDIM`（30）。模型/二进制复用 `_MODEL` / `_CLAUDE_BIN`。
  - **单测**：四类触发表驱动（阈值/空闲/兜底/cooldown/首轮）；整理引擎写盘+清空 / 0600 / `IsPoisoned` 双过滤 / 每维度上限 / 空收件箱 no-op / summarize 失败保留收件箱 / **读-清空 vs 后到 append 竞态** / 二次幂等 / JSON 容错切片；worker 集成（RecordTurn 触发阈值沉淀跑在同一 worker）。顺带修正 63e80fc「默认 ON」遗留的两处 `config_test.go` 失效断言。
- **管家长期记忆 — turn 末蒸馏要点 append 进 workspace CLAUDE.md (ADR-021 §4 v1, #83)**：给「管家」(`RoleButler`) 会话加持久长期记忆，让管家 `claude -p --resume`(cwd=workspace) 重启/换机后仍记得用户偏好与在做的项目。
  - **写入机制（engine 内部步骤，不新增 caller hook）**：`butler.Engine` 收尾点在【`Role==RoleButler && turnEnded && errorCount==0`】时，经新增窄接口 `Deps.MemoryWriter.RecordTurn(userText, replyText, cwd)` **非阻塞**投递本轮。选 engine 内部步骤而非 `Hooks.OnTurnComplete`：通知/reply 路径都不带 `req.Text`(ADR-020 §4)，且蒸馏对 LOCAL/CLOUD 完全相同，不该按 caller 注入。`MemoryWriter==nil`(默认) 整步跳过。
  - **记忆落点（唯一）**：workspace `CLAUDE.md` 的 `<!-- BEGIN/END BBClaw-managed -->` 托管段，复用 `workspace.ReplaceManagedBlock`。**砍掉 ADR-020 的 `memory.json` 注入层**（§1/§2/§4 在管家模式下 Superseded by ADR-021）；各项目 cwd 的 CLAUDE.md 项目画像仍是独立轴。
  - **蒸馏管线（`internal/butler/memory`）**：后台**单 worker(并发=1)** 起 Haiku `claude -p` 把本轮蒸馏成 JSON delta（用户长期偏好 / 最近项目 / 关键决策三类）→ **deny 过滤**（含 `ignore previous`/`system prompt`/`bypass`/`你现在是…` 等指令式条目整条丢，防注入持久化）→ **hash 去重**（幂等）→ **≤4KB FIFO clamp**（防膨胀）→ **原子写 0600**。门控：跳过错误轮 / 过短 utterance / 队列满即丢。失败全吞(log)，绝不阻塞 turn 返回设备。
  - **安全分级**：env `BBCLAW_BUTLER_MEMORY_DISTILL` 默认 **off**（链路 smoke 前不写）；LOCAL 灰度开；**cloud 多租户 v1 不注入写入**（user 维度落地前避免串写）。`BBCLAW_BUTLER_MEMORY_MODEL` / `BBCLAW_BUTLER_MEMORY_CLAUDE_BIN` 可覆盖模型与二进制。
  - **单测**：marker splice append / hash 去重幂等 / ≤4KB FIFO clamp / deny 过滤命中 / 0600 / 原子写无副作用；engine 投递门控（管家 vs 非管家 vs 错误轮 vs nil）/ writer 非阻塞满即丢 / 过短跳过 / env 默认 off。Haiku 真链路属外部 CLI 依赖，留集成冒烟。
- **管家会话路由 — 设备 turn 永远路由到 workspace 管家会话 + `--mcp-config` + WarmPool 预热 (ADR-021 v1, #80)**：设备每次语音/文本 turn 不再自选 driver/session，统一路由到该设备专属的「管家」逻辑会话（`Role=butler`、`cwd=~/.bbclaw-adapter/workspace/`、`driver=claude-code`），管家靠 cwd 自动加载 workspace 的 CLAUDE.md 人设/记忆。
  - **管家会话解析**（`logicalsession.Manager.EnsureButler`）：按 `deviceID+driver` 幂等解析/创建管家会话；首次铸造 `RoleButler`，后续复用以保留会话连续性。每设备一个管家。
  - **路由落点**：`httpapi/agent.go handleAgentMessage`（local）与 `homeadapter/adapter.go handleChatTextViaAgent`（cloud 语音）在配置了 butler workspace 时，忽略设备请求的 driver/session，改喂管家会话走 `butler.Engine.RunTurn`。语音路径从「手撸 `drv.Start`/事件循环」统一到 butler 引擎，经 `voiceEventSink` 适配回 `voice.reply.delta`/`tool_call` 帧，云端协议帧序列不变。未配置 workspace 时（如单测）保持旧多会话行为不破。
  - **`--mcp-config`（仅管家）**：`agent.StartOpts` 新增 `MCPConfig` 字段（契约同 `Model`/`SystemPrompt`，不支持的 driver 忽略）；claudecode `sessionFlags` 在非空时拼 `--mcp-config <path>`。`butler.Engine` 仅当解析到的会话 `Role==butler` 时注入 `Deps.ButlerMCPConfig`，worker/普通会话不带。`butlermcp.WriteConfig` 生成指向 `mcp-server` 子命令（#79）的 stdio MCP 配置文件，启动时写到 `~/.bbclaw-adapter/butler-mcp.json`。
  - **WarmPool 预热管家**：`claudecode.WarmPool` 从单 `warmCwd` 扩成多 `warmCwds`（项目 cwd + 管家 workspace），每 cwd 独立维持 `size` 个预热条目，`Acquire(cwd)` 严格按 cwd 命中——管家每轮命中预热，避免 4-7s 冷启动。
- **管家 MCP 派发 server — worker runner + `mcp-server` 子命令 (ADR-021 v1)**：补齐「管家 MCP 派发」最后一公里。
  - **`ClaudeWorkerRunner`**（`adapter/internal/butlermcp/runner_claude.go`）：落地 `WorkerRunner` 接口，复用 claudecode driver 在目标 cwd 起 worker（`--permission-mode acceptEdits`），消费 stream-json 累积 assistant 文本到 `EvTurnEnd`，`EvError` 透传为错误，输出超长时按头尾保留、中间省略裁剪（默认 8KB，避免把超长 transcript 回灌管家）。
  - **`bbclaw-adapter mcp-server` 子命令**（`adapter/internal/cmd/mcpserver.go`）：从 env 读 `BBCLAW_CWD_POOL`（allowlist）与 `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`，装配 `ClaudeWorkerRunner` + `butlermcp.New`，在 stdio 上 `Serve`；**stdout 仅 JSON-RPC，日志全部走 stderr**（`obs.NewLoggerTo`）。新增 `config.LoadButlerEnv` 仅加载管家所需字段，跳过 ASR/TTS 校验。
  - **e2e 冒烟**（`adapter/scripts/butler-mcp-smoke.sh` + `butler-mcp-config.example.json`）：协议层冒烟无需真实 claude；`BBCLAW_BUTLER_LIVE=1` 时跑 `claude -p --mcp-config` 真链路（claude 不在 PATH 时自动跳过，CI 不依赖）。
- **逻辑会话 Role 字段 + worker 不进设备菜单 (ADR-021, #82)**：为"设备 ↔ 管家(butler) ↔ N 个 worker"会话分层打底。`logicalsession.LogicalSession` 新增 `Role` 字段（`butler` / `worker` / 空=向后兼容的普通会话）及 `RoleButler`/`RoleWorker`/`RoleNone` 常量；`Manager.CreateWithRole` 支持指定角色（`Create` 保持原签名、默认空 role，现有 5 个调用点零改动）。新增 `Manager.ListDeviceFacing`（在 limit 之前先剔除 worker），4 处设备朝向入口改用它：`httpapi` 的 sessions 菜单与 `handleAgentSessionsLogical`、`homeadapter/agent_proxy` 的 cloud relay 列表与菜单两处镜像逻辑——确保 cloud 模式下 worker 也不泄漏给设备。底层 `List` 仍返回全量供 butler/dispatch 使用。旧无 Role 记录反序列化为空 role 仍按普通会话列出；角色的实际写入由 #80(butler)/#79(worker) 消费。

## [0.4.4] - 2026-05-29

### Fixed
- **电池电量显示不准 (P0)**：第一版线性映射（3300mV→0%，4200mV→100%）不符合锂电池放电曲线，导致满电掉电飞快、中段卡住、低电量突然归零。`bb_power.c` 改为 OCV–SoC 放电曲线查表 + 线性插值，并新增三项滤波：(1) 跨周期 EMA 电压低通（`BBCLAW_POWER_EMA_ALPHA_PCT`，默认 25%）吸收 PTT/功放/WiFi 负载瞬态；(2) 百分比迟滞（`BBCLAW_POWER_HYSTERESIS_PCT`，默认 2%）消除 ±1% 抖动；(3) ADC dummy read 规避高阻分压首读偏低。曲线表与方案见 `firmware/docs/feat/power-management-foundation.md`。充电检测仍为 TODO（需 VBUS 脚）。
- **CHAT 最外层长按 OK 进不了菜单**：新 PCB rev 把 OK 移到编码器按压（IO1），`bb_nav_input` 把长按 OK 改发 `BB_NAV_EVENT_BACK` 替代旧的 `OK_LONG`，但 `bb_radio_app` CHAT 状态里"进 SETTINGS"那条路径还挂在 `OK_LONG` 上，导致长按落到空 BACK case 上没反应。把进 SETTINGS 行为合并到 BACK 处理里——busy 时维持取消 in-flight turn 的旧语义，空闲时进入 SETTINGS 浮层。

### Added
- **TTS 阅读模式 + Chat 本地 tail 缓存 (ADR-017)**：解决两个 UX 痛点。
  - **阅读模式**：TTS 播报中按 UP 翻看历史不再被下一句 chunk 拉回底部 — chat transcript 加了 `follow_tail` 锁存，UP 即进入阅读模式，DOWN 滚回底部自动恢复 follow，期间底栏显示 "● 阅读中 (DOWN 到底回到实时)" 提示。
  - **Chat tail 缓存**：每个 driver 在 NVS 里维护一个 1.5KB 的最近消息环（key `cc/<驱动短码>`），睡眠/唤醒回到 chat 时先用本地 cache 渲染最近几条消息，再 fire adapter fetch；adapter 不在线也能看到刚才的对话。Fetch 成功后清缓存重写以保持远端为 SoT。
- **TTS 文本清洗 `tts.Sanitize`**：`/v1/tts/synthesize` 在送入 provider 之前先剥掉 markdown 加粗/斜体/反引号、代码块围栏、ATX 标题、列表/引用前缀、`[文本](链接)`、HTML 标签、零宽与控制字符，并把多行/多空白塌缩成单空格。`say` 之前会把 `**Sonnet 4.5**` 念成"星号星号 Sonnet 4.5"、反引号包路径段也会让发音断裂，清洗后这些都按正常文本播报，保证一段完整内容能跑完。日志里同时带 `text_chars`（清洗后）与 `raw_chars`（原始）便于排查。
- **设备端 Driver / Model 选择** (ADR-016)：Settings 屏改为二级菜单（主屏 Driver/Model/TTS/Back，OK 进同名 picker 子屏），用户可在 BBClaw 上直接切换 active driver（claude-code / opencode / aider / ollama / openclaw）和当前 driver 的 model（Sonnet / Opus / Haiku / GPT-5 / Ollama tags 等），无需 SSH 进 adapter 改 env。Adapter 持久化 `~/.bbclaw-adapter/driver_state.json`，多设备共享。
  - Adapter HTTP：`GET /v1/agent/drivers` 扩展返回 `active_driver` + 每 driver 的 `models[]` 与 `active_model`；新增 `PUT /v1/agent/active_driver` 与 `PUT /v1/agent/drivers/{name}/active_model`
  - Cloud envelope：扩展 `agent.drivers` payload；新增 `agent.active_driver.set` / `agent.active_model.set` kind（cloud_saas 模式下需 cloud 侧配套补 HTTP proxy 才能在远端生效；local_home 模式立即可用）
  - Driver 接口：可选 `ModelLister` 接口；claude-code / opencode / aider 提供静态模型列表，ollama 调 `/api/tags` 60s 缓存；`agent.StartOpts.Model` 字段把选定 model 注入 session 启动参数
  - Firmware：新增 NVS key `drv/active`、`bb_agent_set_active_driver` / `bb_agent_set_active_model`；chat 屏 driver fallback 改读 NVS
  - **UI 修订（2026-05-17）**：Settings 从最初的"扁平 5 行 + LEFT/RIGHT 循环值"改为"二级菜单 + 旋钮 + OK + BACK"，因为硬件实际无 LEFT/RIGHT 物理键（首版方案误用了代码里的残留事件）；Session Picker 去除原 Driver 切换行，driver 选择统一收归 Settings，两个 UI 重复入口消除。

## [0.4.3] - 2026-05-16

### Fixed
- **Adapter home_site_id 在 macOS 上每次重启变化**: 原来用 hostname + user + machine-id 派生 UUID v5，macOS 没有 `/etc/machine-id` 且 hostname 随网络切换变化，导致每次启动生成不同 ID，cloud 端认为是新设备并重新要求 claim。改为首次启动时生成随机 UUID v4 并持久化到 `~/.bbclaw-adapter/identity.json`，后续从文件读取，彻底消除漂移。

## [0.4.1] - 2026-04-27

First release using the unified `release.yml` workflow — firmware OTA bin
and adapter binaries (5 platforms) ship together from a single `v*` tag.

### Added
- **Buddy-anim theme** (Phase 4.6.x): LVGL-driven 9-state animations on top of the
  existing ASCII faces — opacity pulse for SLEEP, y-bob for IDLE/LISTENING,
  dot cycle for BUSY, x-sway for SPEAKING, transform-scale heartbeat for HEART,
  color lerp for ATTENTION, y-bounce + festive color for CELEBRATE,
  fast x-shake for DIZZY. Selectable via NVS like the existing themes.
- **Aider driver** (Phase 4.10): adapter-side driver wrapping the `aider` CLI in
  non-interactive mode (`--message ... --yes-always --no-pretty --no-stream`),
  with per-session `--chat-history-file` for multi-turn continuity. Auto-enabled
  when `aider` is on PATH; overridable via `AGENT_AIDER_FORCE`.
- **Adapter source open-sourced** (ADR-011): the Go adapter moved from
  `bbclaw-reference/adapter` to this repo at `adapter/`, preserving git
  history (subtree split). Module path `github.com/daboluocc/bbclaw/adapter`
  is unchanged. Cloud backend and web portal stay closed.

### Fixed
- **PTT → LISTENING state**: pressing PTT now reliably transitions the buddy face
  to LISTENING. Previous code gated the state change behind WiFi / TTS guards
  that could silently skip it (only the haptic fired); recording still started
  via the arm path, leaving the user without visual feedback.
- **Double-OK kills settings**: rapid OK presses no longer cause the settings
  overlay to flash open then immediately close. The nav-event drain skips
  buffered OK/BACK events for one tick after `settings_overlay_enter()` succeeds.
- **Settings overlay panic-rebooted from chat**: `bb_ui_settings_show()` did
  synchronous NVS reads from the caller's task. The chat→OK→settings path
  runs on `stream_task`, which is allocated on PSRAM stack; NVS internally
  disables SPI flash cache and asserts the stack is in internal RAM, so
  the device hard-rebooted the moment the user opened settings from chat.
  Fix: new `bb_ui_settings_preload_nvs()` is called once from
  `bb_radio_app_start()` (internal-RAM stack), and `load_*_from_nvs()` are
  now idempotent — later calls from any task become pure memory reads.
- **Capture ringbuf overflow on cold-start**: the first HTTP chunk of a PTT
  utterance takes 500–700 ms (TCP connect + first write); the 8-chunk
  (~480 ms) capture ringbuf was just under that ceiling and dropped audio
  at the start of every utterance. Bumped to 32 chunks (~1.9 s in PSRAM).
- **Adapter Makefile ldflags** silently failed to inject build tag/time —
  the `-X` package path was the wrong module name. Pre-existing bug in the
  closed repo, fixed during the migration.

## [0.4.0] - 2026-04-27

### Added
- **Multi-Driver Agent Bus**: support CLI-based agent drivers — Claude Code, OpenCode, Ollama — alongside original OpenClaw
- **LEFT/RIGHT quick driver switching**: carousel-style driver cycling from chat overlay with session auto-reset
- **Agent Chat as home screen**: boot directly into Agent Chat overlay; 90s idle timeout exits to standby
- **Cancel in-flight turn**: OK/BACK during agent thinking cancels the turn (discard events, kill TTS, show IDLE)
- **Chat transcript scrolling**: UP/DOWN scrolls the agent chat transcript (2 lines/press) within the overlay
- **PTT voice bridge**: PTT-to-Agent-Bus pipeline — record → ASR → agent → streaming TTS reply
- **Streaming TTS**: sentence-level TTS with cancel-and-replace on new user turn
- **Buddy-ASCII theme**: seven-state animated character alongside chat transcript
- **Flipper 6-button navigation**: UP/DOWN/LEFT/RIGHT/OK/BACK mapped to 5-way nav module
- **Standalone Settings overlay**: OK opens Settings from chat; driver/theme/TTS toggle persisted to NVS
- **Async driver fetch**: pre-warm driver list on chat entry; non-blocking HTTP cache
- **Device identity in Agent Bus URLs**: deviceId query param for cloud proxy routing

### Fixed
- **PSRAM-stack NVS crash**: NVS reads spawned on internal-RAM task to avoid `cache_utils` assert when `stream_task` (PSRAM stack) triggers SPI flash
- **Driver switch HTTP 400**: clear `session_id` on driver cycle so adapter creates fresh session
- **NVS write deferred**: `cycle_driver` NVS persist offloaded to background task (avoids SPI-flash cache collision)
- **TTS task stack**: bumped 4K → 8K for Phase 4.5.2 streaming pipeline
- **PTT → BUSY transition**: immediate BUSY state on PTT release before cloud-wait
- **Block driver cycling while busy**: LEFT/RIGHT blocked during agent turn in flight

## [0.3.5] - 2026-04-16

### Added
- **GitHub Actions CI**：推送 tag 时自动构建并上传固件到 OTA 服务器

## [0.3.4] - 2026-04-16

### Added
- **OTA 在线升级**：云端连接成功后自动检查并下载固件更新
- **双分区 OTA**：支持 ota_0/ota_1 交替升级，2.5MB 分区空间
- **OTA 状态机**：`bb_ota.c/h` 实现检查/下载/校验/烧写完整流程
- **升级庆祝**：更新成功后首次启动显示"更新成功!"画面
- **固件版本上报**：设备信息包含固件版本，云端可查看

### Fixed
- 分区表：添加缺失 otadata 分区，修正 ota_0 起始地址
- JSON 解析：`hasUpdate` 字段偏移修复 (11→12)
- Makefile flash 地址：0x110000 → 0x120000

## [0.3.3] - 2026-04-15

### Added
- 固件状态机重构：新增 `bb_status.h` 集中定义所有 status 字符串常量
- 状态机文档：`design/STATE_MACHINE.md` 完整描述 AP/锁屏/正常/待机/问答模式
- 状态转换追踪：LOCKED ↔ UNLOCKED 切换时输出 `STATE_TRANSITION` 日志

### Changed
- 重构 `bb_radio_app.c`、`bb_lvgl_display.c`、`bb_display_bitmap.c` 使用 BB_STATUS_* 常量

## [0.3.2] - 2026-04-13

### Fixed
- Adapter WS 心跳：每 25 秒发送 ping，彻底解决 35 秒断连重连循环
- Adapter 并发写安全：所有 `conn.WriteJSON` 统一走带 `sync.Mutex` 的 `writeConn`，防止并发写崩溃

### Changed (Web)
- Home Adapter 详情页展示 Adapter 版本号、运行平台、构建时间

## [0.3.1] - 2026-04-13

### Added
- Adapter 连接云端后自动上报版本号、平台、构建时间（Portal Home Adapter 页可查看）

## [0.3.0] - 2026-04-13

### Added
- Web 对话（Web Chat）：登录后可在 Portal 直接通过浏览器与 OpenClaw 对话，无需持有 BBClaw 硬件
- 流式输出：回复逐字流式显示（SSE），支持停止按钮
- 对话历史：每次会话结果持久化存储，切换设备时自动加载最近 50 条
- Adapter 新增 `chat.text` 请求类型，文字直接转发至 OpenClaw，跳过 ASR 步骤

## [0.1.0] - 2026-04-02

### Added
- 固件开源（Apache-2.0），ESP32-S3 + ES8311 + ST7789 全链路
- PTT 实时语音采集与上传
- 异步通知推送与轻量摘要展示
- WiFi 局域网连接模式
- HTTP 配对码流程
- LVGL 显示界面与 UI 资源
- Adapter / Cloud 运行面集成
- 本地 ASR 工具（FunASR）
- 架构文档、协议规范、硬件引脚与 BOM 文档

### Fixed
- 配对 HTTP 栈稳定性、TTS 采样率、配对码语音播报
- 设备码 JSON 排序稳定性、HTTP body 大小限制
- Makefile 生成目标整合、显示与文档清理
