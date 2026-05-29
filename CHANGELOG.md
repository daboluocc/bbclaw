# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
