# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Removed
- **设备端 CWD picker + new-session 死代码整链清理**：ADR-021 v2 砍掉 session picker 后，CWD picker（issue #30）唯一入口（session picker 的「+ 新建 session」行）随之断链，整条链路不可达：CWD picker overlay UI 与按键路由、设备端 new-session worker（`spawn_new_session_task` 等）、客户端 API `bb_agent_create_session` / `bb_agent_list_cwd_pool`（`POST /v1/agent/sessions` / `GET /v1/agent/cwd-pool` 设备侧调用）、`bb_display_set_cwd_name` 及 `format_relative_time`。设备 turn 已由 adapter `EnsureButler` 强制路由管家会话，设备端不再创建 session。共删 ~650 行（6 个源文件 + 3 个头文件）。

### Fixed
- **待机页直接按 PTT 无「正在聆听」动画/震动反馈**：`stream_task` 的 STANDBY→CHAT 唤醒分支先消费了 PTT 版本号再 `continue`，下一轮循环只剩 `s_ptt_pressed` 电平驱动的 arm 录音路径，唯一触发 LISTENING 状态 + 录音波形遮罩 + 按下震动的 `chat_voice` 边沿分支被整个跳过——录音在跑但屏幕毫无反馈。修复：`agent_chat_enter()` 成功后不再吞边沿，落穿到同一迭代的 `chat_voice` 分支，行为与已在 CHAT 页按 PTT 完全一致（含 busy/adapter-offline 防护）。
- **固件编译失败**：(1) `bb_ui_agent_chat.c` 一处旧注释缺 `*/` 结尾，与下一行注释嵌套触发 `-Werror=comment`；(2) `bb_lvgl_display.c` / `bb_page_locked.c` 底栏 `"mem: %d+%d"` 在 int 极值下可能截断 24 字节 buffer 触发 `-Werror=format-truncation`，inbox/profile 显示值钳制到 999。
- **电池电量显示不准 (P0)**：第一版线性映射（3300mV→0%，4200mV→100%）不符合锂电池放电曲线，导致满电掉电飞快、中段卡住、低电量突然归零。`bb_power.c` 改为 OCV–SoC 放电曲线查表 + 线性插值，并新增三项滤波：(1) 跨周期 EMA 电压低通（`BBCLAW_POWER_EMA_ALPHA_PCT`，默认 25%）吸收 PTT/功放/WiFi 负载瞬态；(2) 百分比迟滞（`BBCLAW_POWER_HYSTERESIS_PCT`，默认 2%）消除 ±1% 抖动；(3) ADC dummy read 规避高阻分压首读偏低。曲线表与方案见 `firmware/docs/feat/power-management-foundation.md`。充电检测仍为 TODO（需 VBUS 脚）。
- **CHAT 最外层长按 OK 进不了菜单**：新 PCB rev 把 OK 移到编码器按压（IO1），`bb_nav_input` 把长按 OK 改发 `BB_NAV_EVENT_BACK` 替代旧的 `OK_LONG`，但 `bb_radio_app` CHAT 状态里"进 SETTINGS"那条路径还挂在 `OK_LONG` 上，导致长按落到空 BACK case 上没反应。把进 SETTINGS 行为合并到 BACK 处理里——busy 时维持取消 in-flight turn 的旧语义，空闲时进入 SETTINGS 浮层。

### Added
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
