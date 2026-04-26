# ADR-006: Flipper 6-button 完整事件 + LEFT/RIGHT 语义

- **日期**: 2026-04-26
- **状态**: 已接受
- **关联**: ADR-005（openclaw 接入 AgentDriver）；扩展 Phase 4.x Option A 的过渡设计

## 背景

Phase 4.x（Option A）把 breadboard 从旋转编码器换成 Flipper Zero 风格的 6 个独立按键，但**事件 API 没动**：UP/DOWN 借用 `BB_NAV_EVENT_ROTATE_CCW/CW`，OK 借用 `CLICK`，BACK 借用 `LONG_PRESS`，LEFT/RIGHT 仅 debounce + log，不发任何事件。

这是一个 0 改菜单代码的过渡方案，让"按键工作"和"UX 升级"解耦：硬件改造完即可在真机验证按键，无需重写 picker / settings。

代价：

- LEFT/RIGHT 是死键，4 个事件叠加 6 个键，UX 词汇贫乏
- 编码器思维（CCW/CW）和按键思维（UP/DOWN）混着用，新人读代码困惑
- 任何"快捷切 driver"的需求都得绕进 Settings 子菜单

## 决策

### 1. 扩 `bb_nav_event_t` 加 6 个语义事件

```c
typedef enum {
  BB_NAV_EVENT_UP = 0,
  BB_NAV_EVENT_DOWN,
  BB_NAV_EVENT_LEFT,
  BB_NAV_EVENT_RIGHT,
  BB_NAV_EVENT_OK,
  BB_NAV_EVENT_BACK,
  BB_NAV_EVENT_COUNT,

  /* 旧名作 alias，数值相同 → 既有 switch/case 不用动 */
  BB_NAV_EVENT_ROTATE_CCW = BB_NAV_EVENT_UP,
  BB_NAV_EVENT_ROTATE_CW  = BB_NAV_EVENT_DOWN,
  BB_NAV_EVENT_CLICK      = BB_NAV_EVENT_OK,
  BB_NAV_EVENT_LONG_PRESS = BB_NAV_EVENT_BACK,
} bb_nav_event_t;
```

**关键约束**：alias 放在 `BB_NAV_EVENT_COUNT` 之后，所以
`s_nav_event_versions[BB_NAV_EVENT_COUNT]` 数组大小是 6，不会被 alias 重复
计数到 10。语义映射规则：

| 编码器/3 键模式发的事件 | Flipper 6 键模式发的事件 | 数值 |
|---|---|---|
| `ROTATE_CCW` | `UP`    | 0 |
| `ROTATE_CW`  | `DOWN`  | 1 |
| (无)         | `LEFT`  | 2 |
| (无)         | `RIGHT` | 3 |
| `CLICK`      | `OK`    | 4 |
| `LONG_PRESS` | `BACK`  | 5 |

调用方按场景选名字：编码器代码 / 兼容性写老名字，6-button 代码写新名字，
两边等价无歧义。

### 2. 6-button 模式直发新事件

`bb_nav_input.c` 的 Flipper 分支按下边沿发 `UP / DOWN / LEFT / RIGHT / BACK`，
释放边沿发 `OK`（保持 click "tap-to-fire" 语义）。编码器 / 3-按键-当编码器
模式继续发旧名字（= 新名字 alias），无需修改。

### 3. LEFT/RIGHT 语义：picker 模式快捷切 driver

参考 Flipper Zero 主菜单的"水平方向 = 同级切换"惯例，确定如下分配：

| 上下文 | LEFT | RIGHT |
|---|---|---|
| Agent Chat picker 模式 | cycle driver -1 | cycle driver +1 |
| Agent Chat settings 模式 | 忽略（保留给未来"行内值微调"） | 忽略 |
| 主屏 / 历史滚动 | 忽略（保留给未来"段落跳转 / 翻页"） | 忽略 |

**为什么 driver 而不是 theme**：driver 影响"AI 是谁"，是用户最频繁的切换；
theme 影响视觉，调一次就稳定下来，进 Settings 改足够。

**为什么 wrap 而不是 clamp**：LEFT/RIGHT 是"轮播"手势，到边界继续转应当回到
另一端（比 picker 上下滚的"碰边"更自然）。Settings 内部仍 clamp，那是确定性
菜单不是轮播。

### 4. 新增公共 API：`bb_ui_agent_chat_cycle_driver(int delta)`

- 复用 Settings 的 `driver_cache`（懒加载，首次同步 HTTP）
- wrap-around；持久化到 NVS；调主题 `set_driver` 立即更新 topbar
- 调用方约束：必须在 LVGL 锁内 + chat 必须 active + 必须在 picker 模式
- `radio_app` 端用 `agent_chat_cycle_driver_locked` 做 lvgl-lock 包装

## 后果

### 正面

- LEFT/RIGHT 物理键终于有用了（之前只是 log）
- 切 driver 从 4 步（BACK 进 chat → 选 Settings → 旋到 Agent → click）变 1 步
- 代码自文档化：写 `BB_NAV_EVENT_BACK` 比 `BB_NAV_EVENT_LONG_PRESS` 直观得多
  （在 Flipper 上 BACK 不是长按，是独立键）
- 编码器板子（生产 PCB）继续用 `ROTATE_CCW / CW / CLICK / LONG_PRESS`
  无任何改动，无破坏性

### 负面 / 风险

- `BB_NAV_EVENT_COUNT` 从 4 变 6，`s_nav_event_versions` 数组大 50%（极小代价）
- 有人读 alias 后困惑"为啥两个名字都能用"——靠注释 + 这份 ADR 说清
- LEFT/RIGHT cycle 第一次会触发 settings_ensure_driver_cache 的同步 HTTP
  （阻塞 LVGL 任务）。Phase 4.2.5 把那个 fetch 改 async 后自动好

### 未来工作

- Phase 4.2.5：driver fetch 改 async；LEFT/RIGHT 第一次调用不再卡 LVGL
- 可能的 Phase 6+：LEFT/RIGHT 在主屏 = 翻页 / 跳段，让 6 按键真正物尽其用
- 添加 transient toast（"switched to: claude-code"）让 cycle 反馈更显眼，
  目前只靠 topbar 文字变化

## 触发何时撤销

- 如果用户实际使用中 LEFT/RIGHT 误触率高（在主屏想做别的事），考虑改成
  长按或 LEFT+RIGHT 组合手势触发 cycle。但目前 picker 模式下方向键专用，
  误触概率可控。
