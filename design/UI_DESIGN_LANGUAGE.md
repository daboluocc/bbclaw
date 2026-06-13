# BBClaw UI 设计语言 — 点阵 / Nothing-style

> 本文档是全机 UI 视觉的唯一真相来源。代码侧落地为
> `firmware/include/bb_ui_theme.h`，**任何颜色/点阵几何改动先改这里，再同步头文件**。
> 起源：开机动画（STATE_MACHINE.md §3.5）、网络连接页（§3.5.1）、待机时钟三页
> 确立的视觉语言，2026-06 全机推广（v0.4.6）。

## 1. 设计原则

1. **单色 + 单一强调**：界面以深近黑底 + 冷白/冷灰的单色层次表达，青色
   （teal `0x2ec4a0`）是唯一的强调色——高亮、下划线、呼吸点、选中态。
   绿/红仅保留充电/错误的语义色，不做装饰。
2. **点阵优先**：图形元素优先用圆点阵列（dot-matrix）画法表达——时钟数字、
   wordmark、WiFi 弧、锁形、VU 柱。dot 5px / pitch 9px / 5×7 字模是基准网格
   （小型元素可缩到 dot 4px / pitch 7px）。
3. **点亮节奏**：动画点亮遵循「最新元素青色闪 → 下一拍沉淀为冷白」的统一节拍
   （开机扫列、netconn 弧、VU 峰值点同语言）。
4. **硬切换页**：全屏页之间硬切，**禁止全屏 opa fade**——parent-opa 会让 LVGL
   走临时全屏合成层，曾与 esp_wifi_init 的内部 DMA 申请冲突造成 NO_MEM
   boot loop（STATE_MACHINE.md §3.5）。
5. **品牌分层色（青 = bbclaw · 黄 = daboluo 云）**：青 `0x2ec4a0` 是 **bbclaw 全线**
   （固件 + adapter 管理面板 + 配套 Web）的官方唯一强调色，代表「本地 / 硬件层」。
   云端 daboluo.cc / SaaS 门户走**同一套点阵体系、但把 accent 映射成黄**
   （`oklch(0.86 0.17 96)`，hue 96°，定义于 `bbclaw-reference/web`，作用域 `.platform-app`/`/app/*`），
   代表「云 / SaaS 层」。二者只差 hue 一个通道（明度/饱和不变），共同构成
   「**本地 = 青 · 云 = 黄**」的双层品牌叙事，对应 thin-device + thick-adapter + cloud 架构分层。
   bbclaw 侧任何 Web 页面（含新 adapter 对话页）一律用青，**不得借用云端黄**；反之亦然。
   （确认日期 2026-06-13。）

## 2. Token 表

| Token | 值 | 用途 |
|-------|-----|------|
| `BB_UI_BG` | `0x070b0e` | 全机唯一屏幕底色（含遮罩底色） |
| `BB_UI_DOT_LIT` | `0xdfeaec` | 点阵亮点 / 主文字（冷白） |
| `BB_UI_DOT_GHOST` | `0x152128` | 点阵 ghost / 分隔线 / assistant 气泡面 |
| `BB_UI_TEXT_DIM` | `0x6e8a93` | 次级文字（冷蓝灰） |
| `BB_UI_WORDMARK` | `0x4f6f67` | 页脚 wordmark（暗青灰） |
| `BB_UI_ACCENT` | `0x2ec4a0` | 唯一强调青 |
| `BB_UI_OK` | `0x4cd964` | 充电 / 成功（语义色） |
| `BB_UI_ERR` | `0xe66f6f` | 低电 / 错误（语义色） |
| `BB_UI_MX_DOT` | `5` | 基准点直径 px |
| `BB_UI_MX_PITCH` | `9` | 基准点距 px |

**已废弃**（v0.4.6 起不得出现在 firmware/src）：
`0x0a0e0c`（旧偏亮底色）、`0xd8ebe4`（旧主文字，并入 DOT_LIT）、
`0x7a9a8c` / `0x8fbcac`（暖绿次级文字，并入 TEXT_DIM）、
`0x4a9fd8`（assistant 蓝，气泡单色化后删除）、
`0xf0e6c8` / `0xa49a83` / `0xffd166` / `0xff8fd0`（buddy 暖色，收敛三色）、
`0xffffff`（Settings 选中纯白，并入 DOT_LIT）。

**跨层色相对照（OKLCH 单通道，见 §1 原则 5）**：本体系 Web 落地用 OKLCH 表达，
层间只变 hue：bbclaw 青 ≈ `oklch(0.82 0.13 175)`（≙ 固件 `0x2ec4a0`）；
daboluo 云黄 = `oklch(0.86 0.17 96)`。Web 侧 `:root` 取值见 `dot-matrix-ui` skill
（其唯一真相源即本文档）。

## 3. 各页面应用

| 页面 | 文件 | 点阵 motif | 备注 |
|------|------|-----------|------|
| 开机动画 | `bb_page_boot.c` | BBCLAW 字模逐列扫亮 + 青下划线 | §3.5 |
| 网络连接 | `bb_page_netconn.c` | WiFi 弧（基点+3 层点弧）逐层点亮 | §3.5.1 |
| 待机时钟 | `bb_page_standby.c` | 5×7 点阵数字 + 青色呼吸冒号 | 无时间显示居中横杠 |
| 锁屏 | `bb_page_locked.c` | 点阵锁形（弧形 shackle + 点阵 body + 青色 keyhole 呼吸点）| VERIFY 时 keyhole 呼吸加速；失败 body 闪红一拍 |
| ~~录音遮罩~~ | ~~`bb_chat_recording.c`~~ | **已移除**（2026-06）——全宽 320×112 遮罩每 48ms 重绘 7 条 meter 是活动态最贵的 LVGL 重绘源，dispatch 经 lv_async 排在其后，录音起点/状态视觉滞后 ~240ms。录音指示统一由下方 ACTIVE 底栏 `BAR_LISTEN`(VU) 表达 | — |
| ACTIVE 底栏点阵带 | `bb_lvgl_display.c` | 3×N 点阵小屏，**按状态切 motif**：处理/忙碌=扫描sweep（整列彗星）· 聆听=声波vu（**跟随真实麦克风电平**，`bb_display_set_record_level`）· 说话=行波wave · 错误/DIZZY=红脉冲pulse · 待机=呼吸breathe。chat 模式下由 agent 状态直接驱动（`bb_display_set_agent_bar_state`，因 chat 不更新 legacy status）；非 chat 由 status 字符串驱动 | dot 4 / pitch 7 / vpitch 5；取代旧 `[B] cwd \| mem:N+M` 文字（仍在锁屏 footer）；与 daboluo.cc/style 的 `dot-matrix-anim.js` 同源 motif 库 |
| 聊天气泡 | `bb_chat_transcript.c` | —（色块） | user=青 30%，assistant=ghost 面+冷白字，tool=ghost 10%+DIM，error=ERR |
| ACTIVE 顶栏 | `bb_lvgl_display.c` | —（文字+位图图标） | 文字 DIM，活跃元素 ACCENT；底栏见上方扫描条 |
| Settings / 任务列表 | `bb_ui_settings.c` / `bb_ui_task_list.c` | —（列表） | 选中行 = ghost 行面 + 青色左缘 3px 竖条 + LIT 文字 |

## 4. 变更流程

新页面 / 改样式：先在本文档登记 token 与 motif → 同步 `bb_ui_theme.h` →
代码只允许引用 token（不得新增裸 hex 颜色，语义色除外需在 §2 注册）。
