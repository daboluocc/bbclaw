# ADR-007: 独立 Settings overlay（Phase 4.7）

- **编号**: ADR-007
- **标题**: 把 Settings 从 Agent Chat 子模式提升为独立全屏 overlay
- **日期**: 2026-04-27
- **状态**: 已接受
- **关联**: ADR-005（openclaw driver 接入）；ADR-006（Flipper 6-button）；Phase 4.2.5 → 4.7 演进

## 背景

Phase 4.2.5 的初版 Settings 是 **Agent Chat picker 模式内的一个子模式**：

- chat overlay 顶部 topbar + 主体 picker（聊天历史 / driver 列表）
- 底部条（bottom strip）放一行 Settings 入口，文字主题（text-only）下尤其明显
- 用户在 picker 模式时，UP/DOWN 翻条目；想进 Settings 还得旋到底部那行 → click
- Settings 内 UP/DOWN 切行（driver / theme / 待扩展），LEFT/RIGHT 改值

这个设计当时图省事：复用了 chat overlay 的渲染管线、布局栅格、按键路由。但真机用一周后用户的反馈很直接：

> "Settings 藏得太深了，我每次切 driver 都要先 BACK 进 chat、滚到底、再 click，再
> UP/DOWN 找到 Agent 那行，再 LEFT/RIGHT。一个简单的切换搞了 5 步。"

且 ADR-006 已经把 LEFT/RIGHT 在 picker 模式分配给了**快捷切 driver**，"行内 LEFT/RIGHT 改值"的语义只对 Settings 模式有效，跨模式心智负担陡增。Settings 不该再寄生在 Agent Chat 里。

## 决策

**把 Settings 从 Agent Chat 子模式抽成独立的全屏 overlay**，主屏 OK 召唤、与 Agent Chat overlay 互斥。

### 1. UI 入口与互斥

| 触发 | 行为 |
|---|---|
| 待机屏幕（standby）按 OK | 进入 Settings overlay |
| Settings 内按 BACK | 退出回待机屏 |
| Agent Chat overlay 已开 | OK 不会进 Settings（Agent Chat 自己处理 OK） |
| Settings overlay 已开 | PTT / 任何 nav 事件不会触发 Agent Chat 自动召唤（见 ADR-008） |

两个 overlay **在任意时刻最多只有一个可见**，由 `bb_radio_app` 的 stream task 维护互斥不变量。

### 2. 文件结构

```
firmware/include/bb_ui_settings.h    # 公共 API
firmware/src/bb_ui_settings.c        # 实现：渲染 + 输入 + NVS 持久化
firmware/src/bb_radio_app.c          # overlay enter/exit 包装、互斥仲裁
```

公共 API 形态（伪签名）：

```c
esp_err_t bb_ui_settings_init(void);
bool      bb_ui_settings_is_active(void);
void      bb_ui_settings_enter(void);     /* 必须在 lvgl_port_lock 内 */
void      bb_ui_settings_exit(void);      /* 必须在 lvgl_port_lock 内 */
void      bb_ui_settings_handle_nav(bb_nav_event_t ev);
```

### 3. 输入语义（关键转折点）

Phase 4.7.1 一度尝试**LEFT/RIGHT 立即改值并立即落 NVS**，理由是"所见即所得"。
真机灰屏崩溃。原因复盘见下节。

最终 Phase 4.7.2 收敛到 **OK = commit** 模型：

| 键 | 行为 |
|---|---|
| UP/DOWN  | 切换当前选中行（driver / theme / …） |
| LEFT/RIGHT | **预览**当前行的候选值（仅改内存中的 staging value，不写 NVS、不调 driver/theme apply） |
| OK | **提交**：把 staging value 落 NVS、调用对应 setter 立即生效 |
| BACK | 放弃未提交的预览，关闭 overlay |

### 4. 已尝试但放弃：自动保存（Phase 4.7.1）

实现了 LEFT/RIGHT 触发时直接 `nvs_set_str` + `bb_agent_theme_set_driver()`。在
**LVGL render 任务持锁、TTS 同时占用 SPI 总线**的瞬间触发 NVS 写入，导致：

- NVS 写发生在 lvgl_port_lock 临界区内
- NVS commit 触发 flash 擦写（耗时 ~30ms）
- 同窗口内 LVGL render tick + I2S DMA 耗光看门狗预算
- 任务监视器（`task_wdt`）超时 → 设备重启

**根因**：把"重 IO（flash 写）"和"重渲染（LVGL）"摆在同一锁的同一帧。

Phase 4.7.2 的修法：

- LEFT/RIGHT 路径**只改内存**，O(1)
- 只有用户**显式 OK** 时才 commit；OK 已经天然是低频事件（用户主动间隔 ≥ 数百
  毫秒），与下一帧 LVGL render 之间留出了让 flash 写完的窗口
- 即便如此，commit 路径也释放 lvgl_port_lock 后再调 NVS API（仅在锁外做 IO）

这条经验抽象出来就是：**lvgl_port_lock 内做的事必须是纯内存操作**。flash、网络、阻塞 syscall 全部禁止。后续所有 overlay 实现都必须遵守。

### 5. 后续 commit 链

| commit | 阶段 | 内容 |
|---|---|---|
| `763fd0a` | Phase 4.7   | 抽 Settings 为独立 overlay，OK 召唤 |
| `ab6d886` | Phase 4.7.1 | 实验自动保存（LEFT/RIGHT 立即落 NVS）—— 后回滚 |
| `6a7d25c` | Phase 4.7.2 | 回到 OK-to-commit；LEFT/RIGHT 仅预览 |

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 维持 Phase 4.2.5 子模式不变 | UX 反馈明确"太深"；ADR-006 已抢走 picker LEFT/RIGHT 语义，子模式 LEFT/RIGHT 含义变得歧义 |
| 把 Settings 做成弹窗（modal）叠在主屏上 | 仍要叠在 LVGL 渲染层；与 Agent Chat overlay 一样要走互斥逻辑；不如直接做成 overlay 一致性更好 |
| 多个 Settings 子页（分页） | 当前条目只有 driver / theme 两行，分页过度设计 |
| 自动保存 + 即时落 NVS（Phase 4.7.1 走过） | 触发 task_wdt 重启；硬伤，已回滚 |

## 后果

### 正面

- 切 driver / theme 从 5 步降到 3 步（OK 进 Settings → UP/DOWN 选行 → LEFT/RIGHT/OK）
- Settings 与 Agent Chat 的代码彻底解耦，互不污染各自的输入路由
- LEFT/RIGHT 预览 + OK 提交是干净的"事务"模型，未来加更多行（model 选择、TTS 音量、idle timeout 时长）只需复制现有行实现
- 建立了"lvgl_port_lock 内禁止 IO"的项目级硬规则，被 ADR-008 等后续工作复用

### 负面 / 风险

- 主屏 OK 现在专属 Settings，再想加"主屏快捷动作"得重新设计（目前主屏只是 standby，无别的动作可加，影响有限）
- 与 Agent Chat overlay 的互斥矩阵手工维护，加 overlay 数会增长成 N×N（目前 N=2，远未到痛点）

### 未来工作

- 加更多 Settings 行：
  - 模型选择（claude-code 的 Sonnet / Haiku；ollama 的具体模型名）
  - 语音 idle timeout 时长（替代 ADR-008 的硬编码 90s）
  - PTT 灵敏度 / VAD 阈值
  - TTS 音色 / 语速
- 分组（section）：driver-related vs voice-related vs display-related
- 提交时给一个 toast 反馈（"saved: driver = openclaw"），让 OK 的"submit"语义更明显
- 考虑把 ADR-009 的 Agent State 在 Settings 标题栏显示一个图标，让"切 driver
  时当前 agent 在干嘛"对用户透明
