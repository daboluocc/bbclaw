# LimeZu 待机吉祥物（LVGL 元素）

待机界面右下角循环播放 **Green** 的 Idle 四帧（32×32，RGB565，由 `gen-lvgl-elements` 生成）。

**Red / Blue** 的 Idle 四帧已一并编入 `bb_lvgl_element_assets`（符号 `bb_el_red_idle_0..3`、`bb_el_blue_idle_0..3`），当前 UI 未使用，便于后续 Kconfig / 主题切换。

## 来源与许可

见 [assets/third_party/limezu/README.md](../assets/third_party/limezu/README.md)。

## 更新切片

从 itch 解压目录重新切图并刷新 LVGL 资源：

```bash
cd firmware
python3 scripts/slice_limezu_mini_idle.py --src /path/to/Free\ Mini\ Characters --out ui/lvgl/elements
make gen-lvgl-elements
```

整体 Figma / PNG 工作流见 [figma_lvgl_workflow.md](figma_lvgl_workflow.md)。
