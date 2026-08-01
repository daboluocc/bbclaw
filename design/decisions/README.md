# 设计决策记录 (ADR)

记录重要的架构和设计决策。

## 记录格式

每个决策应包含：
- **编号**: ADR-XXX
- **标题**: 简洁描述
- **日期**: YYYY-MM-DD
- **状态**: 已接受 / 已废弃 / 已替代
- **背景**: 决策前的状况
- **决策**: 具体的决策内容
- **后果**: 采用后的影响

## 决策列表

| 编号 | 标题 | 日期 | 状态 |
|------|------|------|------|
| [ADR-001](ADR-001-adapter-as-agent-bus.md) | adapter 作为 Agent 总线 | 2026-04-25 | 已接受 |
| [ADR-002](ADR-002-multi-turn-session-lifecycle.md) | 多轮会话生命周期 | 2026-04-25 | 已接受 |
| [ADR-003](ADR-003-router-and-multi-driver.md) | Router + 多 driver 路由策略 | 2026-04-25 | 已接受 |
| [ADR-004](ADR-004-cloud-agent-proxy.md) | cloud_saas 模式下的 Agent Bus 代理 | 2026-04-25 | 已接受 |
| [ADR-005](ADR-005-openclaw-as-driver.md) | openclaw 接入 AgentDriver（重评 ADR-001） | 2026-04-26 | 已接受 |
| [ADR-006](ADR-006-flipper-full-nav-events.md) | Flipper 6-button 完整事件 + LEFT/RIGHT 语义 | 2026-04-26 | 已接受 |
| [ADR-007](ADR-007-standalone-settings-overlay.md) | 独立 Settings overlay（Phase 4.7） | 2026-04-27 | 已替代（→ ADR-012） |
| [ADR-008](ADR-008-chat-as-standby-and-idle-exit.md) | Chat 作为待机首页 + 90s 空闲退出（Phase 4.8.x） | 2026-04-27 | 已替代（→ ADR-012） |
| [ADR-009](ADR-009-agent-state-machine.md) | Agent 9 态状态机：LISTENING / BUSY / SPEAKING（Phase 4.8.x） | 2026-04-27 | 已接受 |
| [ADR-010](ADR-010-per-device-agentdriver-cloud-config.md) | Per-device AgentDriver 作为云配置（v0.4.0 多 driver） | 2026-04-27 | 已接受 |
| [ADR-011](ADR-011-adapter-open-source.md) | Adapter 开源（搬到主仓） | 2026-04-27 | 已接受 |
| [ADR-012](ADR-012-fixed-page-menu.md) | 固定三页菜单（Standby / Chat / Settings）取代 overlay 召唤 | 2026-04-30 | 已接受 |
| [ADR-013](ADR-013-session-history-replay.md) | 设备端会话历史回放与上下翻页 | 2026-05-04 | 已接受 |
| [ADR-014](ADR-014-logical-session-abstraction.md) | Logical Session 抽象——把 CLI session 细节移出设备 | 2026-05-04 | 已接受 |
| [ADR-015](ADR-015-device-monitor-over-usb.md) | Device Monitor over USB（截图 + 按键注入） | 2026-05-12 | 已实现 |
| [ADR-016](ADR-016-device-driver-model-selection.md) | 设备端 Driver / Model 选择（Settings 双行 + Adapter 持久化） | 2026-05-17 | 已接受（已实现） |
| [ADR-017](ADR-017-tts-reading-mode-and-chat-cache.md) | TTS 阅读模式 + Chat tail 缓存 | 2026-05-18 | 已实现 |
| [ADR-018](ADR-018-device-butler-architecture.md) | 设备管家(Butler)——Adapter 作为会话编排 + 记忆 + Claude 完整适配中枢 | 2026-05-30 | 已接受（实施中） |
| [ADR-019](ADR-019-server-driven-menu-protocol.md) | Server-Driven 菜单协议——把设备 picker 渲染下沉到 adapter | 2026-05-30 | 已接受（待实现） |
| [ADR-020](ADR-020-memory-pipeline.md) | 记忆管线——用户需求记忆 + 本地项目画像（复用 Claude 原生） | 2026-05-30 | 草案（v1 范围明确，蒸馏延后） |
| [ADR-021](ADR-021-conversational-orchestrator-butler.md) | 对话式编排管家——Claude 管家会话 + MCP 派发到 worker CLI Agent | 2026-05-30 | 草案（方向已定，2 个 spike 闸门） |
| [ADR-022](ADR-022-memory-consolidation-and-profile-docs.md) | 记忆整理——收件箱 + 画像文档两层记忆（consolidation 归档进 workspace/MEMORY/） | 2026-06-02 | 已接受（v1 范围明确，5 项决策定稿） |
| [ADR-021-firmware-ui](ADR-021-firmware-ui.md) | 管家模式固件 UI——Task List / 派发状态注入 / 底部状态栏 / PTT 文案 | 2026-05-30 | 草案（docs-only） |
| [ADR-023](ADR-023-driver-management-and-butler-driver.md) | 驱动管理——通用驱动 / 独立 butler 驱动 / 环境检测接入页面 | 2026-06-10 | 已接受（v1 范围明确） |
| [ADR-024](ADR-024-multi-driver-butler-ecosystem.md) | 多-driver 自包含管家生态——persona 投影 + 同源 worker + 随 driver 的记忆 | 2026-06-10 | 已接受（设计定稿，分期实现） |
| [ADR-025](ADR-025-web-first-configuration.md) | Web 优先配置——把 .env 搬上管理页，分系统/AI/对话/数据四页 | 2026-06-10 | 已接受（v1 范围明确） |
| [ADR-026](ADR-026-butler-onboarding.md) | 管家初次见面(onboarding)——确定性注入而非 persona 软指引 | 2026-06-11 | 已接受 |
| [ADR-027](ADR-027-device-home-adapter-switching.md) | 设备端切换 Home Adapter（切机器）——cloud 组装的设备态选择器 | 2026-06-12 | 提议（待评审） |
| [ADR-028](ADR-028-conversation-core-v2.md) | Conversation Core v2——Turn 模型 / 全状态打断 / 6 态收敛 / sink 驱动字幕对齐 | 2026-06-13 | 提议（待评审） |
| [ADR-029](ADR-029-conversation-page-structured-parts.md) | 对话页结构化 parts——thinking / text / tool / dispatch 分段回放 | 2026-06-13 | 已接受 |
| [ADR-030](ADR-030-device-execution-steps.md) | 设备端执行步骤——display-only step 通道（说主结果、显步骤） | 2026-06-17 | 已接受 |
| [ADR-031](ADR-031-opencode-as-canonical-backend.md) | OpenCode 作为 canonical 后端——收敛 driver 动物园，serve + SDK 取代 scrape-CLI | 2026-06-17 | 草案（方向已定，1 个 spike 闸门） |
| [ADR-032](ADR-032-adapter-v2-conversation-sessions.md) | adapter_v2 会话生命周期——默认续接 / 新对话 / 历史选择 / 按会话隔离 | 2026-06-23 | 提议（P1 续接 + PTT 退出已落地，P2/P3 待实现） |
| [ADR-033](ADR-033-adapter-v2-blocking-prompt-device-confirm.md) | adapter_v2 阻塞式交互弹窗 → 设备确认（截屏识别 + 转发选择） | 2026-06-24 | 进行中（四端已实现，待真机端到端验证） |
| [ADR-034](ADR-034-adapter-v2-task-list-channel.md) | adapter_v2 计划清单（TodoWrite）→ 设备 display-only `task.list` 通道 | 2026-06-25 | 草案（docs-only，待 P0 抓屏识别器闸门） |
| [ADR-035](ADR-035-adapter-v2-interactive-pty-over-claude-p.md) | adapter_v2 跑交互式 PTY + 抓屏而非 `claude -p`——计费留在订阅内 + 多 CLI 无缝兼容 | 2026-06-25 | 已接受（追认既有架构） |
| [ADR-036](ADR-036-adapter-v2-project-loader.md) | adapter_v2 项目载入——管理页登记目录 + 项目清单注入系统提示（re-exec 落地）+ CLI 发现 | 2026-06-25 | 进行中（P1 核心已实现：projectstore + CRUD + 系统提示 + 原生目录对话框 + 用户画像展示 + 设备控制） |
| [ADR-037](ADR-037-firmware-miyu-settings-toggle.md) | 固件设置菜单密语(miyu)开关——cloud_saas 下可设备端开/关锁屏语音解锁，重启生效 | 2026-06-25 | 已接受（已实现，`make build` 通过；待真机验证 + owner 发布） |
| [ADR-038](ADR-038-firmware-miyu-show-recognized-text.md) | 密语解锁失败显示识别到的语音文本——纯固件消费云端已回的 transcript，锁屏「听到「…」」 | 2026-06-25 | 已接受（已实现，`make build` 通过；待真机验证 + owner 发布） |
| [ADR-043](ADR-043-no-shared-agent-driver-sdk.md) | 不抽公共 Agent 驱动 SDK——bbclaw adapter 与 agent-room 两边独立实现（stream-json 已是互通标准，PTY 独家只设备需要） | 2026-07-01 | 已接受（决定不做） |
| [ADR-048](ADR-048-device-adapter-allocation.md) | 设备级 Adapter 分配——后台白名单管控设备可见/可切的 adapter 集合（空集合=全可见，固件零改动） | 2026-07-19 | 提议（待评审） |
| [ADR-049](ADR-049-device-image-capture-multimodal.md) | 摄像头拍照→多模态 Agent——实战派 OV2640 单帧 JPEG 走 base64 文本 envelope relay 到 adapter 喂 claude 读图（cloud 零改动，硬骨头=摄像头 bring-up） | 2026-07-19 | 提议（待评审） |
| [ADR-050](ADR-050-firmware-wifi-outage-recovery-reprovision.md) | WiFi 断网恢复与重新配网——运行期四级阶梯（快速重试→退避→全网轮换→门户回落）+ 免重启热切网 + 设置页网络入口（纯固件，cloud/adapter 零改动） | 2026-08-01 | 提议（待评审） |
