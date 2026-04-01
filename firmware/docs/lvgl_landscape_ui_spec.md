# BBClaw 1.47 ST7789 Landscape UI Spec

目标：

- 为 `320x172` 横屏 ST7789 定一套可落地的简约 UI
- 先支持本地 LVGL 联调，不被图片导出阻塞
- 后续平滑接入 Figma 官方导出素材

设计源：

- Figma 文件：[BBClaw ST7789 Landscape UI](https://www.figma.com/design/YB7P6zFrpKncMwhvz9dvXf)

## 设计原则

1. `内容优先`
   小屏面积有限，优先保证状态、语音、任务、历史等核心信息一眼可读。
2. `固定骨架`
   所有 screen 共用 `顶部状态栏 + 中部主体 + 底部辅助信息`。
3. `图片节制`
   图片只承担固定视觉元素，不承担频繁变化的文本和值。
4. `本地先跑`
   第一阶段先用 LVGL 原生组件把结构跑起来，再补 Figma 导出元素。
5. `RGB565 友好`
   控制大面积透明、发光和叠层，避免 ST7789 上刷新和观感变差。

## 视觉语言

主题是 `minimal HUD`：

- 深色底
- 低饱和辅助色
- 清晰状态色条
- 轻量几何装饰
- 不使用重阴影、拟物、厚玻璃层

推荐色值，和当前 LVGL 实现保持一致：

- `bg/screen`: `#0A0E0C`
- `bg/panel-me`: `#141C18`
- `bg/panel-ai`: `#121820`
- `accent/me`: `#2EC4A0`
- `accent/ai`: `#4A9FD8`
- `text/main`: `#D8EBE4`
- `text/dim`: `#7A9A8C`
- `text/status`: `#8FBCAC`

## Screen Set

### 01-standby

Figma node: `1:22`

用途：

- 待机
- 在线状态
- 当前时间
- Wi-Fi / idle 辅助信息

布局：

- 顶部是状态栏
- 左侧大字号时间
- 右侧 hero mark / claw 标识
- 底部一行环境信息

动态：

- 时间
- 状态文案
- Wi-Fi 名称
- idle / ready 状态

静态候选：

- hero mark
- 顶部电池框

### 02-notification

Figma node: `1:53`

用途：

- 收到新任务或消息卡片

布局：

- 顶部状态栏 + `new` tag
- 中部消息卡片
- 左侧头像底板
- 右侧姓名 + 摘要
- 底部 `open / dismiss`

动态：

- 发送者
- 摘要文本
- 任务状态

静态候选：

- 头像底板
- 卡片轮廓装饰

### 03-history

Figma node: `1:85`

用途：

- 最近 3 条历史消息预览

布局：

- 顶部标题和条数
- 下方 3 行 history row
- 每行含头像圆角块、姓名、摘要、时间

动态：

- 列表内容
- 时间
- 条目数量

静态候选：

- 无需整页图
- 如需视觉统一，只保留共享头像底板

### 04-speaking

Figma node: `1:141`

用途：

- PTT 按住时的讲话 / 录音 / 上传中状态

布局：

- 顶部状态栏 + `ptt`
- 左侧话筒或 claw hero 区
- 右侧主提示语
- 底部波形条或音量条

动态：

- 主提示语
- streaming 状态
- 波形 / 音量条

静态候选：

- 话筒底板
- hero 装饰底图

最佳实践：

- 波形不要导出逐帧图
- 用 `lv_bar`、`lv_line`、短条形 `lv_obj` 或遮罩 reveal 做动画

### 05-settings

Figma node: `1:179`

用途：

- 显示几个最常用配置项

布局：

- 顶部状态栏
- 中部 3 个 settings row
- 左侧标题和说明
- 右侧 value chip

动态：

- 网络状态
- 音量数值
- 开关状态

静态候选：

- value chip 轮廓可由 LVGL 画
- 不建议导出整页或整行图片

### 06-about

Figma node: `1:226`

用途：

- 展示设备身份、版本、能力摘要

布局：

- 顶部状态栏
- 左侧品牌/设备底板
- 右侧标题和说明
- 底部显示 firmware 和 panel 信息

动态：

- 版本号
- 节点状态
- 设备信息

静态候选：

- hero mark
- 品牌底板

## 组件边界

以下内容优先由 LVGL 原生组件承担：

- 顶部状态栏布局
- 所有文本
- 所有列表行
- settings row
- footer 辅助信息
- speaking 波形或电平条
- 数值变化、状态切换、选择焦点

以下内容适合后续从 Figma 导出：

- `claw` 标识
- 少量共享几何装饰
- speaking 页固定底板
- notification 页头像底板
- 电池轮廓或特殊状态图标

## 推荐导出资产

第一批建议只导出共享元素，不导出整页：

- `el_claw_mark_88.png`
- `el_avatar_plate_64.png`
- `el_mic_plate_88.png`
- `el_battery_frame.svg`
- `el_signal_mark.svg`

第二批再评估是否需要整页背景：

- `bg_standby_base.png`
- `bg_notification_base.png`
- `bg_about_base.png`

`history`、`settings`、`speaking` 默认不建议依赖整页 PNG。

## 本地联调顺序

1. 用 LVGL 原生组件实现 6 个 screen 的骨架
2. 保持文本、列表、数值全部动态
3. 只接入少量共享 icon / 底板图
4. 验证字号、换行、刷新性能
5. 等 Figma 视觉稳定后，再导出正式元素图

## 交付边界

当前这套方案的目标不是“先烤出一堆图片”，而是：

- 先把 screen 结构和资产边界定清楚
- 让 LVGL 开发不等素材也能开始
- 保证后续 Figma 导出能无缝接入
