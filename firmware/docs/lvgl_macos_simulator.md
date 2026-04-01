# LVGL macOS 本地模拟方案

更新时间：`2026-03-24`

## 结论

当前 BBClaw 在 macOS 上的本地预览主线应该是：

- `LVGL + SDL2` 原生渲染
- 当前项目内置的 `firmware/simulator/`
- 先在 Mac 上调布局、字体、中文换行、状态切换
- 最后再上真机收物理窗口和偏移

`SquareLine Studio` 适合设计、导出和快速搭界面，但它不是当前 BBClaw 运行态预览的主线。

## 官方查询结果

### 1. LVGL 官方 macOS 方案

LVGL 官方当前明确给出 `macOS` 页面，说明 macOS 上的 ready-to-use project 使用的就是 `SDL Driver`。官方还列了两个现成入口：

- `lv_port_pc_vscode`
- `lv_port_pc_eclipse`

来源：

- <https://docs.lvgl.io/master/integration/pc/macos.html>
- 页面标注最后更新时间：`Mar 23, 2026`

### 2. LVGL 官方 SDL Driver

LVGL 官方 `SDL Driver` 文档明确写了：

- macOS 依赖安装：`brew install sdl2`
- `lv_conf.h` 里打开 `LV_USE_SDL 1`
- 基本用法是 `lv_init()` 后调用 `lv_sdl_window_create(...)`
- 完整 demo 可以直接跑 `lv_demo_widgets()`
- 默认后端是标准软件渲染
- 也支持 `SDL Draw Unit`
- 也支持 OpenGL / NanoVG 路线

来源：

- <https://docs.lvgl.io/master/integration/pc/sdl.html>
- 页面标注最后更新时间：`Mar 23, 2026`

### 3. SquareLine Studio

SquareLine Studio 当前文档版本为 `1.6.0`，支持 macOS 安装。它更适合：

- 设计界面
- 导出 LVGL 项目代码
- 做设计稿级别预览

但对于 BBClaw 当前需求，它不能替代“项目内真实 UI 运行态模拟器”。

来源：

- <https://docs.squareline.io/docs/introduction/install/>
- 页面标注版本：`ver. 1.6.0`

## BBClaw 当前实现

项目内已落地的 macOS 模拟器路径：

- [README.md](/Volumes/1TB/github/bbclaw/firmware/simulator/README.md)
- [CMakeLists.txt](/Volumes/1TB/github/bbclaw/firmware/simulator/CMakeLists.txt)
- [main.c](/Volumes/1TB/github/bbclaw/firmware/simulator/main.c)
- [lv_conf.h](/Volumes/1TB/github/bbclaw/firmware/simulator/lv_conf.h)

当前 simulator 特点：

- 目标窗口尺寸固定为 `320x172`
- 直接复用固件里的真实 UI 实现（共享 `src/bb_lvgl_display.c`）
- 复用 BBClaw 当前横屏 UI 的 safe area
- 支持 `standby / notification / speaking` 三态
- `speaking` 阶段已支持原生 LVGL 条形波形动画
- 支持离屏导图 `--export`
- 支持批量导图 `make sim-export-feedback`
- 支持中文字体，前提是存在 `generated/lv_font_bbclaw_cjk.c`
- 当前约定是：设备端可做物理旋转，但 simulator 和导出的 PNG 保持正常阅读方向

## 当前推荐工作流

1. 在 Figma 里设计静态元素
2. 导出 PNG/SVG 进入 LVGL 资源链路
3. 在 macOS simulator 里调页面结构和动态表现
4. 最后上 ST7789 真机收口

## 本地命令

构建：

```bash
cd /Volumes/1TB/github/bbclaw/firmware
make sim-build
```

运行：

```bash
cd /Volumes/1TB/github/bbclaw/firmware
make sim-run
```

单张导图：

```bash
cd /Volumes/1TB/github/bbclaw/firmware
make sim-run SIM_ARGS='--mode speaking --status TX --export /tmp/bbclaw-speaking.png'
```

批量反馈图：

```bash
cd /Volumes/1TB/github/bbclaw/firmware
make sim-export-feedback
```

默认输出目录：

```text
/Volumes/1TB/github/bbclaw/firmware/.cache/sim-feedback/
```

推荐闭环：

1. 改 `src/bb_lvgl_display.c`
2. 跑 `make sim-export-feedback`
3. 直接看 PNG 收版式
4. 最后再烧录看真机偏移、颜色和黑边

## 什么时候用哪个

- 要看 BBClaw 当前项目真实 UI：用项目内 `simulator`
- 要看 LVGL 官方标准 demo：单独加 `lv_demo_widgets()` 或 `lv_demo_benchmark()`
- 要做可视化设计和代码导出：用 `SquareLine Studio`
