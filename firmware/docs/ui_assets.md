# BBClaw 公共 UI 资源（Icon / Logo）

目标：

- 为固件提供可复用的公共图形元素层
- 图形风格统一，后续页面可直接引用，不再各自硬编码
- 风格上致敬 Flipper Zero 的简洁像素感

## 资源分层

1. SVG 源文件（设计源）

- 目录：`assets/svg/`
- 作用：统一维护 icon/logo 的矢量版本

2. 固件运行时位图（渲染源）

- 声明：`include/bb_ui_assets.h`
- 数据：`src/bb_ui_assets.c`
- 格式：1-bit monochrome（按行 bit-packed）

## 已内置元素

- `BB_UI_LOGO_CLAW_16`
- `BB_UI_ICON_READY_12`
- `BB_UI_ICON_TX_12`
- `BB_UI_ICON_RX_12`
- `BB_UI_ICON_ERR_12`
- `BB_UI_ICON_TASK_12`
- `BB_UI_ICON_SPEAK_12`
- `BB_UI_ICON_REC_12_1`
- `BB_UI_ICON_REC_12_2`
- `BB_UI_ICON_REC_12_3`

## 接入方式

显示层可直接通过 `bb_ui_mono_bitmap_t` 渲染。当前 `bb_display.c` 已将状态文本映射到统一 icon：

- READY -> ready icon
- TX / RX -> 方向 icon
- ERR -> error icon
- TASK / WIFI / ADAPTER -> task icon
- SPEAK -> speak icon

## 新增元素建议流程

1. 在 `assets/svg/` 先设计或更新 SVG
2. 导出目标尺寸的 1-bit 位图
3. 同步到 `src/bb_ui_assets.c` 并在 `include/bb_ui_assets.h` 暴露符号
4. 在页面代码中仅引用公共符号，不再复制位图数据
