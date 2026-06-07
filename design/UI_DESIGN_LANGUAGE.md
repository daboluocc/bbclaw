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

## 3. 各页面应用

| 页面 | 文件 | 点阵 motif | 备注 |
|------|------|-----------|------|
| 开机动画 | `bb_page_boot.c` | BBCLAW 字模逐列扫亮 + 青下划线 | §3.5 |
| 网络连接 | `bb_page_netconn.c` | WiFi 弧（基点+3 层点弧）逐层点亮 | §3.5.1 |
| 待机时钟 | `bb_page_standby.c` | 5×7 点阵数字 + 青色呼吸冒号 | 无时间显示居中横杠 |
| 锁屏 | `bb_page_locked.c` | 点阵锁形（弧形 shackle + 点阵 body + 青色 keyhole 呼吸点）| VERIFY 时 keyhole 呼吸加速；失败 body 闪红一拍 |
| 录音遮罩 | `bb_chat_recording.c` | VU 7 列×5 行点阵柱，bottom-up 点亮，峰值点 voiced 青闪 | dot 4 / pitch 7 |
| 聊天气泡 | `bb_chat_transcript.c` | —（色块） | user=青 30%，assistant=ghost 面+冷白字，tool=ghost 10%+DIM，error=ERR |
| buddy chip | `bb_theme_buddy_anim.c` | —（字符表情） | 九态收敛 LIT/DIM/ACCENT 三色，靠动效区分 |
| ACTIVE 顶/底栏 | `bb_lvgl_display.c` | —（文字+位图图标） | 文字 DIM，活跃元素 ACCENT |
| Settings / 任务列表 | `bb_ui_settings.c` / `bb_ui_task_list.c` | —（列表） | 选中行 = ghost 行面 + 青色左缘 3px 竖条 + LIT 文字 |

## 4. 变更流程

新页面 / 改样式：先在本文档登记 token 与 motif → 同步 `bb_ui_theme.h` →
代码只允许引用 token（不得新增裸 hex 颜色，语义色除外需在 §2 注册）。
