# BBClaw LVGL Simulator

本地预览当前 `1.47" 320x172` 横屏 LVGL UI，不用反复烧录。

当前 simulator 已直接复用固件里的真实 UI 实现：

- 共享 [bb_lvgl_display.c](/Volumes/1TB/github/bbclaw/firmware/src/bb_lvgl_display.c)
- `sim-run` 看交互窗口
- `sim-export-feedback` 批量导出 PNG 做评审
- 设备端当前已做 `180°` 旋转；`simulator` 和导出的 PNG 保持正常阅读方向，方便调版式

依赖：

- macOS
- `brew install sdl2`

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

批量导图：

```bash
cd /Volumes/1TB/github/bbclaw/firmware
make sim-export-feedback
```

默认会导出到：

```text
/Volumes/1TB/github/bbclaw/firmware/.cache/sim-feedback/
```

常用参数：

```bash
make sim-run SIM_ARGS='--mode standby'
make sim-run SIM_ARGS='--mode notification --status TASK --reply "新的任务已经同步"'
make sim-run SIM_ARGS='--mode speaking --status TX'
make sim-run SIM_ARGS='--zoom 4 --timeout-ms 2000'
make sim-run SIM_ARGS='--mode speaking --status TX --export /tmp/bbclaw-speaking.png'
```

说明：

- 模拟器复用了当前横屏 UI 的 safe area、状态栏、`standby / notification / speaking` 三态布局。
- 如果仓库里存在 `generated/lv_font_bbclaw_cjk.c`，模拟器会自动带上中文字体。
- 它能快速校验排版、换行、图片位置。
- `sim-export-feedback` 是当前推荐闭环：改代码 -> 导图 -> 看图继续调。
- 它不模拟 ST7789 的真实面板偏移、物理黑边和颜色差异，最终仍需要真机做一次收口。
- 设备端的物理朝向可和 simulator 不同；当前约定是“设备端旋转，截图不旋转”。

官方参考：

- LVGL macOS: <https://docs.lvgl.io/master/integration/pc/macos.html>
- LVGL SDL Driver: <https://docs.lvgl.io/master/integration/pc/sdl.html>
- SquareLine Studio 安装: <https://docs.squareline.io/docs/introduction/install/>

补充说明：

- `SquareLine Studio` 适合做设计和导出，不是 BBClaw 当前运行态模拟主线。
- BBClaw 当前本地预览主线是 `LVGL + SDL2` 原生渲染。
- `--export` 会把当前页面离屏导出成图片，方便留档和评审。
