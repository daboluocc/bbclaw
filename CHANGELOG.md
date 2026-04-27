# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
